package scheduler

import (
	"time"

	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/config"
	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/service"
	log "github.com/sirupsen/logrus"
)

// Scheduler tunables; overridable in tests.
var (
	// renewalRetryInterval is how soon the scheduler retries when the serving
	// certificate cannot be read (unusable material or a transient error).
	// Panicking here used to brick the webhook: CrashLoopBackOff with an
	// empty-cert Secret surviving restarts, recoverable only by manually
	// deleting the Secret. Retrying lets config.Run repair it in place.
	renewalRetryInterval = 30 * time.Second
	// serviceRestartDelay gives the HTTPS server time to release :8443
	// between Stop and the restart.
	serviceRestartDelay = 2 * time.Second
)

var ticker *time.Ticker

func StartCertRenewalScheduler(cHandler *config.Handler, sHandler *service.Handler, certRenewalPeriod int64) {
	expireDate, err := cHandler.GetCertExpireDate()
	if err != nil {
		log.Errorf("cannot determine certificate expire date (%s), retrying certificate recovery in %s", err.Error(), renewalRetryInterval)
		time.AfterFunc(renewalRetryInterval, func() {
			renewCertificate(cHandler, sHandler, certRenewalPeriod)
			StartCertRenewalScheduler(cHandler, sHandler, certRenewalPeriod)
		})

		return
	}

	currentDate := time.Now().UTC()
	difference := expireDate.Sub(currentDate)
	// we always need 1 min extra because if the expire time is 0 the cert is still valid
	sTime := int64(difference.Minutes()) - certRenewalPeriod + 1
	if sTime < 1 {
		// the ticker cannot be 0 or negative
		sTime = 1
	}
	log.Infof("certificate expires at %s, next renewal check in %d minute(s)", expireDate.Format(time.RFC3339), sTime)

	ticker = time.NewTicker(time.Duration(sTime) * time.Minute)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				log.Infof("certRenewalPeriod is reached, renewing certificate and secret")
				if _, err := cHandler.Run(certRenewalPeriod); err != nil {
					log.Errorf("certificate renewal failed: %s", err.Error())
				}
				restartService(sHandler)
				ticker.Stop()
				StartCertRenewalScheduler(cHandler, sHandler, certRenewalPeriod)
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
}

// renewCertificate repairs the TLS secret and the on-disk serving material via
// config.Run, then restarts the HTTPS server when the material changed or no
// Run goroutine currently owns the listener (e.g. binding failed at startup
// because the files were missing or unusable).
func renewCertificate(cHandler *config.Handler, sHandler *service.Handler, certRenewalPeriod int64) {
	renewed, err := cHandler.Run(certRenewalPeriod)
	if err != nil {
		log.Errorf("certificate recovery attempt failed: %s", err.Error())
	}
	if renewed || !sHandler.IsServing() {
		restartService(sHandler)
	}
}

func restartService(sHandler *service.Handler) {
	if err := sHandler.Stop(); err != nil {
		log.Errorf("Error stopping service during renewal: %v", err)
	}
	// Wait for service to fully stop
	time.Sleep(serviceRestartDelay)
	go sHandler.Run()
}

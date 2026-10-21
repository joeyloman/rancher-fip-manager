package scheduler

import (
	"crypto/tls"
	"time"

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
	// serviceRestartDelay gives a dead HTTPS server time to release :8443
	// between Stop and the rebind. Renewals themselves no longer restart the
	// server: material is swapped in memory via service.SetCertificate.
	serviceRestartDelay = 2 * time.Second
)

var ticker *time.Ticker

// certSource is the slice of config.Handler the scheduler drives; the
// interface lets tests exercise the tick without an API server.
type certSource interface {
	Run(certRenewalPeriod int64) (bool, error)
	ServingCertificate() (*tls.Certificate, error)
	GetCertExpireDate() (time.Time, error)
}

// server is the slice of service.Handler the scheduler drives.
type server interface {
	Run()
	Stop() error
	IsServing() bool
	SetCertificate(pair *tls.Certificate)
}

func StartCertRenewalScheduler(cHandler certSource, sHandler server, certRenewalPeriod int64) {
	expireDate, err := cHandler.GetCertExpireDate()
	if err != nil {
		log.Errorf("cannot determine certificate expire date (%s), retrying certificate recovery in %s", err.Error(), renewalRetryInterval)
		time.AfterFunc(renewalRetryInterval, func() {
			renewCertificate(cHandler, sHandler, certRenewalPeriod)
			StartCertRenewalScheduler(cHandler, sHandler, certRenewalPeriod)
		})

		return
	}

	minutesLeft := int64(time.Until(expireDate).Minutes())
	sTime := renewalCheckIntervalMinutes(minutesLeft, certRenewalPeriod)
	log.Infof("certificate expires at %s (%d minute(s) left), next renewal check in %d minute(s)", expireDate.Format(time.RFC3339), minutesLeft, sTime)

	ticker = time.NewTicker(time.Duration(sTime) * time.Minute)
	go func() {
		// One goroutine per scheduled interval: the tick handler reschedules
		// and returns. The previous loop fell through without returning,
		// leaking a goroutine per renewal.
		for range ticker.C {
			log.Infof("certRenewalPeriod is reached, renewing certificate and secret")
			renewCertificate(cHandler, sHandler, certRenewalPeriod)
			ticker.Stop()
			StartCertRenewalScheduler(cHandler, sHandler, certRenewalPeriod)

			return
		}
	}()
}

// renewalCheckIntervalMinutes computes how long to wait before the next
// renewal check. The raw schedule (lifetime − certRenewalPeriod + 1, the
// "+1" because a cert expiring at minute 0 is still valid) is bounded above
// by min(lifetime/3, certRenewalPeriod): the webhook re-checks at least
// three times per certificate lifetime, and never schedules a check later
// than the operator's configured renewal period. Without the bound an
// oversized CERTRENEWALPERIOD drove the raw value ≤ 0 and the floor of 1
// minute collapsed a perfectly healthy cert into a per-minute renewal
// bounce. cmd/webhook also clamps the period itself.
func renewalCheckIntervalMinutes(minutesLeft, certRenewalPeriod int64) int64 {
	sTime := minutesLeft - certRenewalPeriod + 1

	maxWait := minutesLeft / 3
	if certRenewalPeriod < maxWait {
		maxWait = certRenewalPeriod
	}
	if maxWait < 1 {
		maxWait = 1
	}

	if sTime > maxWait {
		sTime = maxWait
	}
	if sTime < 1 {
		// the ticker cannot be 0 or negative; the recovery path retries on
		// renewalRetryInterval, so a floored tick never busy-loops below 1m
		sTime = 1
	}

	return sTime
}

// renewCertificate runs one renewal attempt. Success is in-place: the fresh
// material is pushed into the running HTTPS server (net/http resolves the
// certificate per handshake), so the admission endpoint keeps serving
// throughout (the old path unconditionally ran Stop →
// sleep(2s) → Run every tick — a ~2-12s write blackout per renewal even
// when nothing had changed). The server is only restarted when it is not
// serving at all, e.g. the listener never bound or died.
func renewCertificate(cHandler certSource, sHandler server, certRenewalPeriod int64) {
	if _, err := cHandler.Run(certRenewalPeriod); err != nil {
		log.Errorf("certificate renewal attempt failed: %s", err.Error())
	}

	if pair, err := cHandler.ServingCertificate(); err != nil {
		log.Errorf("cannot load webhook TLS material for in-place rotation: %s", err.Error())
	} else {
		sHandler.SetCertificate(pair)
	}

	if !sHandler.IsServing() {
		restartService(sHandler)
	}
}

// restartService rebinds a dead HTTPS server (missing material at startup,
// listener error). It is never part of the normal renewal path any more.
func restartService(sHandler server) {
	if err := sHandler.Stop(); err != nil {
		log.Errorf("Error stopping service during recovery: %v", err)
	}
	// Wait for service to fully stop
	time.Sleep(serviceRestartDelay)
	go sHandler.Run()
}

package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/admission"
	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/config"
	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/scheduler"
	"github.com/joeyloman/rancher-fip-manager/pkg/webhook/service"
	log "github.com/sirupsen/logrus"
)

var progname string = "rancher-fip-manager-webhook"

var certRenewalPeriod int64

type appConfig struct {
	logLevel          string
	certRenewalPeriod int64
	kubeConfigFile    string
	kubeConfigContext string
}

func parseAppEnv() *appConfig {
	cfg := &appConfig{}

	logLevel := os.Getenv("LOGLEVEL")
	if logLevel == "" {
		logLevel = "INFO"
	}
	cfg.logLevel = logLevel

	certRenewal, err := strconv.ParseInt(os.Getenv("CERTRENEWALPERIOD"), 10, 64)
	if err != nil || certRenewal == 0 {
		// default the cert renewal expire interval to 30 days
		certRenewal = 30 * 24 * 60
	}
	cfg.certRenewalPeriod = certRenewal

	kubeConfigFile := os.Getenv("KUBECONFIG")
	cfg.kubeConfigFile = kubeConfigFile

	kubeConfigContext := os.Getenv("KUBECONTEXT")
	cfg.kubeConfigContext = kubeConfigContext

	return cfg
}

func init() {
	// Log as JSON instead of the default ASCII formatter.
	formatter := &log.TextFormatter{
		FullTimestamp: true,
	}
	log.SetFormatter(formatter)
	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)
}

func main() {
	cfg := parseAppEnv()

	level, err := log.ParseLevel(cfg.logLevel)
	if err == nil {
		log.SetLevel(level)
	}

	certRenewalPeriod = cfg.certRenewalPeriod

	kubeconfig_file := cfg.kubeConfigFile
	if kubeconfig_file == "" {
		homedir := os.Getenv("HOME")
		kubeconfig_file = filepath.Join(homedir, ".kube", "config")
	}

	kubeconfig_context := cfg.kubeConfigContext

	ctx, cancel := context.WithCancel(context.Background())

	configHandler := config.Register(
		ctx,
		kubeconfig_file,
		kubeconfig_context,
		"rancher-fip-manager-webhook",
		"rancher-fip-manager",
	)

	admissionHandler := admission.Register(
		ctx,
		kubeconfig_file,
		kubeconfig_context,
		"rancher-fip-manager-webhook",
		"rancher-fip-manager",
		"rancher-fip-manager-validator",
	)

	serviceHandler := service.Register(
		ctx,
	)

	configHandler.Init()
	configHandler.Run(certRenewalPeriod)
	admissionHandler.Init()
	scheduler.StartCertRenewalScheduler(configHandler, serviceHandler, certRenewalPeriod)
	go serviceHandler.Run()
	go Run()

	log.Infof("%s is running", progname)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Infof("%s received shutdown signal, gracefully shutting down...", progname)
	cancel()
	os.Exit(0)
}

func Run() {
	for {
		time.Sleep(time.Second)
	}
}

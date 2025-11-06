package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	listers "github.com/joeyloman/rancher-fip-manager/pkg/generated/listers/rancher.k8s.binbash.org/v1beta1"
	"github.com/joeyloman/rancher-fip-manager/pkg/ipam"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

type Server struct {
	listenAddr                   string
	ipamAllocator                *ipam.IPAllocator
	floatingIPPoolLister         listers.FloatingIPPoolLister
	floatingIPProjectQuotaLister listers.FloatingIPProjectQuotaLister
	floatingIPLister             listers.FloatingIPLister
}

func New(
	servicePort string,
	ipamAllocator *ipam.IPAllocator,
	poolLister listers.FloatingIPPoolLister,
	quotaLister listers.FloatingIPProjectQuotaLister,
	fipLister listers.FloatingIPLister,
) *Server {
	return &Server{
		listenAddr:                   fmt.Sprintf(":%s", servicePort),
		ipamAllocator:                ipamAllocator,
		floatingIPPoolLister:         poolLister,
		floatingIPProjectQuotaLister: quotaLister,
		floatingIPLister:             fipLister,
	}
}

func (s *Server) Start(ctx context.Context) {
	server := &http.Server{
		Addr:              s.listenAddr,
		Handler:           s.createHandler(),
		ReadHeaderTimeout: 60 * time.Second,
	}

	go func() {
		logrus.Infof("starting http service on %s", s.listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logrus.Fatalf("failed to start http service: %s", err)
		}
	}()

	<-ctx.Done()
	logrus.Info("shutting down http service")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logrus.Errorf("http service shutdown failed: %s", err)
	}
}

func (s *Server) createHandler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/ipam", s.handleIPAM)
	mux.Handle("/metrics", promhttp.HandlerFor(s.createRegistry(), promhttp.HandlerOpts{}))
	return mux
}

func (s *Server) handleIPAM(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	usage := s.ipamAllocator.GetUsage()
	if err := json.NewEncoder(w).Encode(usage); err != nil {
		logrus.Errorf("failed to encode ipam usage: %s", err)
		http.Error(w, "failed to encode ipam usage", http.StatusInternalServerError)
	}
}

func (s *Server) createRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(NewCollector(s.floatingIPPoolLister, s.floatingIPProjectQuotaLister, s.floatingIPLister))
	return r
}

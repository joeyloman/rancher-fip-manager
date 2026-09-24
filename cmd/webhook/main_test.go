package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseAppEnv(t *testing.T) {
	testCases := []struct {
		name                string
		envVars             map[string]string
		expectedLogLevel    string
		expectedCertRenewal int64
		expectedKubeConfig  string
		expectedKubeContext string
	}{
		{
			name:                "default values",
			envVars:             map[string]string{},
			expectedLogLevel:    "INFO",
			expectedCertRenewal: 43200,
			expectedKubeConfig:  "",
			expectedKubeContext: "",
		},
		{
			name: "custom values",
			envVars: map[string]string{
				"LOGLEVEL":          "DEBUG",
				"CERTRENEWALPERIOD": "60",
				"KUBECONFIG":        "/path/to/kubeconfig",
				"KUBECONTEXT":       "my-context",
			},
			expectedLogLevel:    "DEBUG",
			expectedCertRenewal: 60,
			expectedKubeConfig:  "/path/to/kubeconfig",
			expectedKubeContext: "my-context",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.envVars {
				os.Setenv(key, value)
				defer os.Unsetenv(key)
			}

			cfg := parseAppEnv()

			assert.Equal(t, tc.expectedLogLevel, cfg.logLevel)
			assert.Equal(t, tc.expectedCertRenewal, cfg.certRenewalPeriod)
			assert.Equal(t, tc.expectedKubeConfig, cfg.kubeConfigFile)
			assert.Equal(t, tc.expectedKubeContext, cfg.kubeConfigContext)
		})
	}
}

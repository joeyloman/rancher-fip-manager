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

// TestParseAppEnvCertRenewalPeriod pins REPORT.md 2.4: CERTRENEWALPERIOD is
// minutes with a sane range. Previously only 0 and parse errors fell back
// to the default, so an oversized value (e.g. 1000000 ≈ 694 days on a
// ~1-year cert) drove the renewal tick to its 1-minute floor and bounced
// the admission endpoint every minute.
func TestParseAppEnvCertRenewalPeriod(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int64
	}{
		{"unset uses the 30 day default", "", certRenewalPeriodDefault},
		{"zero uses the default", "0", certRenewalPeriodDefault},
		{"garbage uses the default", "not-a-number", certRenewalPeriodDefault},
		{"sub-minute clamps to the minimum", "30", certRenewalPeriodMin},
		{"negative clamps to the minimum", "-5", certRenewalPeriodMin},
		{"oversized clamps to the maximum", "100000000", certRenewalPeriodMax},
		{"valid value passes through", "43200", 43200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CERTRENEWALPERIOD", c.env)
			assert.Equal(t, c.want, parseAppEnv().certRenewalPeriod)
		})
	}
}

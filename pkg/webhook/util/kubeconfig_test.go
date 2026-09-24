package util

import (
	"testing"
)

func TestGetKubeConfig(t *testing.T) {
	tests := []struct {
		name        string
		kubeConfig  string
		kubeContext string
		wantErr     bool
	}{
		{
			name:        "invalid config file",
			kubeConfig:  "invalid",
			kubeContext: "test",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GetKubeConfig(tt.kubeConfig, tt.kubeContext)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetKubeConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

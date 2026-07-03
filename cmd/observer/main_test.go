package main

import (
	"testing"
	"time"
)

func TestValidateSyncPeriod(t *testing.T) {
	tests := []struct {
		name    string
		in      time.Duration
		wantErr bool
	}{
		{"default ten minutes", 10 * time.Minute, false},
		{"one second", time.Second, false},
		{"zero", 0, true},
		{"negative", -5 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSyncPeriod(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSyncPeriod(%v) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

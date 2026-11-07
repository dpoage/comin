package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadSyncGateOptions(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		command string
		timeout int
		wantErr string
	}{
		{name: "absent", yaml: minimalConfig, command: "", timeout: 0},
		{name: "command only gets the default timeout", yaml: minimalConfig + "sync_gate:\n  command: /x/y\n", command: "/x/y", timeout: 30},
		{name: "both set", yaml: minimalConfig + "sync_gate:\n  command: /x/y\n  timeout: 7\n", command: "/x/y", timeout: 7},
		{name: "timeout zero", yaml: minimalConfig + "sync_gate:\n  command: /x/y\n  timeout: 0\n", command: "/x/y", timeout: 30},
		{name: "timeout without command", yaml: minimalConfig + "sync_gate:\n  timeout: 7\n", command: "", timeout: 7},
		{name: "timeout negative", yaml: minimalConfig + "sync_gate:\n  command: /x/y\n  timeout: -1\n", wantErr: "sync_gate.timeout"},
		{name: "timeout largest", yaml: minimalConfig + "sync_gate:\n  command: /x/y\n  timeout: 9223372036\n", command: "/x/y", timeout: 9223372036},
		{name: "timeout overflows duration", yaml: minimalConfig + "sync_gate:\n  command: /x/y\n  timeout: 9223372037\n", wantErr: "sync_gate.timeout"},
		{name: "command relative", yaml: minimalConfig + "sync_gate:\n  command: x/y\n", wantErr: "sync_gate.command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := Read(writeConfig(t, tt.yaml))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.command, config.SyncGate.Command)
			assert.Equal(t, tt.timeout, config.SyncGate.Timeout)
		})
	}
}

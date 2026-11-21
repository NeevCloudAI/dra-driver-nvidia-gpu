/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const testPCIAddress = "0000:01:00.0"

func TestFbdevHoldsGPU(t *testing.T) {
	tests := map[string]struct {
		fbdev     string // "" leaves nvidia_drm unloaded
		framebuff bool
		want      bool
	}{
		"nvidia_drm not loaded":            {},
		"fbdev disabled":                   {fbdev: "N\n", framebuff: true},
		"fbdev enabled, no framebuffer":    {fbdev: "Y\n"},
		"fbdev enabled, GPU backs console": {fbdev: "Y\n", framebuff: true, want: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			sysModule, pciDevices := filepath.Join(root, "module"), filepath.Join(root, "devices")
			require.NoError(t, os.MkdirAll(filepath.Join(pciDevices, testPCIAddress), 0o755))
			if tc.fbdev != "" {
				params := filepath.Join(sysModule, "nvidia_drm", "parameters")
				require.NoError(t, os.MkdirAll(params, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(params, "fbdev"), []byte(tc.fbdev), 0o644))
			}
			if tc.framebuff {
				require.NoError(t, os.MkdirAll(filepath.Join(pciDevices, testPCIAddress, "graphics", "fb0"), 0o755))
			}

			got, err := fbdevHoldsGPU(sysModule, pciDevices, testPCIAddress)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFbdevHoldsGPUUnreadableParameter(t *testing.T) {
	root := t.TempDir()
	// A directory where the parameter file should be makes the read fail with
	// something other than "not found".
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nvidia_drm", "parameters", "fbdev"), 0o755))
	_, err := fbdevHoldsGPU(root, root, testPCIAddress)
	require.Error(t, err)
}

type evacuation struct {
	calls []string
}

func (e *evacuation) manager(persistErr, drainErr, waitErr error, holders []string, scanErr error) *VfioPciManager {
	return &VfioPciManager{
		disablePersistence: func(string) error {
			e.calls = append(e.calls, "persistence-off")
			return persistErr
		},
		setDrain: func(_ string, draining bool) error {
			if draining {
				e.calls = append(e.calls, "drain")
				return drainErr
			}
			e.calls = append(e.calls, "undrain")
			return nil
		},
		waitForFree: func(context.Context, *VfioDeviceInfo) error {
			e.calls = append(e.calls, "wait-free")
			return waitErr
		},
		findHolders: func(*VfioDeviceInfo) ([]string, error) {
			e.calls = append(e.calls, "rescan")
			return holders, scanErr
		},
	}
}

func TestEvacuateGPU(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]struct {
		persistErr, drainErr, waitErr, scanErr error
		holders                                []string
		wantCalls                              []string
		wantDrained                            bool
		wantErr                                bool
	}{
		"free: waits, then drains and rescans": {
			wantCalls:   []string{"persistence-off", "wait-free", "drain", "rescan"},
			wantDrained: true,
		},
		"still held: refuses without ever draining": {
			waitErr:   boom,
			wantCalls: []string{"persistence-off", "wait-free"},
			wantErr:   true,
		},
		"claimed between the wait and the drain: undrains and refuses": {
			holders:   []string{"4242(nv-hostengine)"},
			wantCalls: []string{"persistence-off", "wait-free", "drain", "rescan", "undrain"},
			wantErr:   true,
		},
		"rescan fails: undrains and refuses": {
			scanErr:   boom,
			wantCalls: []string{"persistence-off", "wait-free", "drain", "rescan", "undrain"},
			wantErr:   true,
		},
		"drain unsupported: falls back to the holder scans": {
			drainErr:  boom,
			wantCalls: []string{"persistence-off", "wait-free", "drain", "rescan"},
		},
		"drain unsupported and claimed again: nothing to undrain": {
			drainErr:  boom,
			holders:   []string{"4242(nv-hostengine)"},
			wantCalls: []string{"persistence-off", "wait-free", "drain", "rescan"},
			wantErr:   true,
		},
		"persistence mode stuck on: stops before waiting": {
			persistErr: boom,
			wantCalls:  []string{"persistence-off"},
			wantErr:    true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			e := &evacuation{}
			vm := e.manager(tc.persistErr, tc.drainErr, tc.waitErr, tc.holders, tc.scanErr)
			drained, err := vm.evacuateGPU(context.Background(), &VfioDeviceInfo{PciBusID: testPCIAddress})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantDrained, drained)
			require.Equal(t, tc.wantCalls, e.calls)
		})
	}
}

func TestEvacuateGPUNamesLateHolders(t *testing.T) {
	e := &evacuation{}
	vm := e.manager(nil, nil, nil, []string{"4242(nv-hostengine)"}, nil)
	_, err := vm.evacuateGPU(context.Background(), &VfioDeviceInfo{PciBusID: testPCIAddress})
	require.ErrorContains(t, err, "4242(nv-hostengine)")
}

func TestGPUHoldersWithoutParent(t *testing.T) {
	holders, err := gpuHolders(&VfioDeviceInfo{PciBusID: testPCIAddress})
	require.NoError(t, err)
	require.Empty(t, holders)
}

func TestUndrainFailureIsOnlyLogged(t *testing.T) {
	called := false
	vm := &VfioPciManager{setDrain: func(_ string, draining bool) error {
		called = !draining
		return errors.New("nvidia-smi gone")
	}}
	vm.undrain(testPCIAddress)
	require.True(t, called)
}

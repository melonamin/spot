package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxSDKExtensionBytes = 8 << 20

// Operator-owned extensions are snapshotted at startup, never read from site uploads.
func loadSDKExtensionAssets(dir string) (map[string][]byte, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir) // ReadDir sorts by filename.
	if err != nil {
		return nil, fmt.Errorf("SDK extensions directory: %w", err)
	}
	assets := make(map[string][]byte)
	remaining := int64(maxSDKExtensionBytes)
	for _, entry := range entries {
		name := entry.Name()
		var asset string
		switch {
		case strings.HasSuffix(name, ".d.ts"):
			asset = "spot.d.ts"
		case strings.HasSuffix(name, ".js"):
			asset = "spot.js"
		default:
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("SDK extension %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("SDK extension %s must be a regular file", name)
		}
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("SDK extension %s: %w", name, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, remaining+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("SDK extension %s: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("SDK extension %s: %w", name, closeErr)
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return nil, fmt.Errorf("SDK extensions exceed %d bytes", maxSDKExtensionBytes)
		}
		if _, ok := assets[asset]; !ok {
			core, err := staticAssets.ReadFile("static_assets/sdk/" + asset)
			if err != nil {
				return nil, fmt.Errorf("SDK extension core %s: %w", asset, err)
			}
			assets[asset] = bytes.Clone(core)
		}
		separator := "\n"
		if asset == "spot.js" {
			separator = "\n;\n"
		}
		assets[asset] = append(assets[asset], separator...)
		assets[asset] = append(assets[asset], data...)
		assets[asset] = append(assets[asset], '\n')
	}
	return assets, nil
}

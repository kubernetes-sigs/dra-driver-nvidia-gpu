/*
 * Copyright (c) NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nvpassthrough

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	vfioPciAliasPrefix string = "alias vfio_pci:"
)

// modAlias is a decomposed version of string like this
//
// vNNNNNNNNdNNNNNNNNsvNNNNNNNNsdNNNNNNNNbcNNscNNiNN
//
// The "NNNN" are always of the length in the example
// unless replaced with a wildcard ("*").
type modAlias struct {
	vendor               string // v
	device               string // d
	subvendor            string // sv
	subdevice            string // sd
	baseClass            string // bc
	subClass             string // sc
	programmingInterface string // i
}

// vfioAlias represents an entry from the modules.alias file for a vfio driver.
type vfioAlias struct {
	modAlias *modAlias // The modalias pattern
	driver   string    // The vfio driver name
}

func parseModAliasString(input string) (*modAlias, error) {
	if input == "" {
		return nil, fmt.Errorf("modalias string is empty")
	}

	input = strings.TrimSpace(input)

	// Trim the leading "pci:" prefix in the modalias file
	_, input, ok := strings.Cut(input, "pci:")
	if !ok {
		return nil, fmt.Errorf("unexpected number of parts in modalias after trimming 'pci:' prefix: %s", input)
	}

	if !strings.HasPrefix(input, "v") {
		return nil, fmt.Errorf("modalias must start with 'v', got: %s", input)
	}
	ma := &modAlias{}
	var before, after string
	var found bool
	after = strings.TrimPrefix(input, "v")

	before, after, found = strings.Cut(after, "d")
	if !found {
		return nil, fmt.Errorf("failed to find delimiter 'd' in %q", input)
	}
	ma.vendor = before

	before, after, found = strings.Cut(after, "sv")
	if !found {
		return nil, fmt.Errorf("failed to find delimiter 'sv' in %q", input)
	}
	ma.device = before

	before, after, found = strings.Cut(after, "sd")
	if !found {
		return nil, fmt.Errorf("failed to find delimiter 'sd' in %q", input)
	}
	ma.subvendor = before

	before, after, found = strings.Cut(after, "bc")
	if !found {
		return nil, fmt.Errorf("failed to find delimiter 'bc' in %q", input)
	}
	ma.subdevice = before

	before, after, found = strings.Cut(after, "sc")
	if !found {
		return nil, fmt.Errorf("failed to find delimiter 'sc' in input %q", input)
	}
	ma.baseClass = before

	before, after, found = strings.Cut(after, "i")
	if !found {
		return nil, fmt.Errorf("failed to find delimiter 'i' in %q", input)
	}
	ma.subClass = before
	ma.programmingInterface = after

	return ma, nil
}

func getKernelVersion() (string, error) {
	var uname unix.Utsname
	if err := unix.Uname(&uname); err != nil {
		return "", err
	}

	// Convert C-style byte array to Go string
	release := make([]byte, 0, len(uname.Release))
	for _, c := range uname.Release {
		if c == 0 {
			break
		}
		release = append(release, c)
	}

	return string(release), nil
}

func getVFIOAliases() ([]vfioAlias, error) {
	kernelVersion, err := getKernelVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get kernel version: %w", err)
	}

	modulesAliasFilePath := filepath.Join(libModulesRoot, kernelVersion, "modules.alias")
	modulesAliasContent, err := os.ReadFile(modulesAliasFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", modulesAliasFilePath, err)
	}

	// Get all vfio aliases from the modules.alias file
	// (all lines starting with 'alias vfio_pci:')
	return getVFIOAliasesFromString(string(modulesAliasContent)), nil

}

// getVFIOAliases returns the vfio driver aliases from the input string.
// The input string is expected to be the content of a modules.alias file.
// Only lines that begin with 'alias vfio_pci:' are parsed, with the
// format being: alias vfio_pci:<modalias string> <driver_name>.
func getVFIOAliasesFromString(input string) []vfioAlias {
	var aliases []vfioAlias

	lines := strings.Split(input, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if !strings.HasPrefix(line, vfioPciAliasPrefix) {
			continue
		}

		split := strings.SplitN(line, " ", 3)
		if len(split) != 3 {
			continue
		}
		modAliasStr := split[1]
		modAlias, err := parseModAliasString(modAliasStr)
		if err != nil {
			continue
		}

		driver := split[2]
		aliases = append(aliases, vfioAlias{
			modAlias: modAlias,
			driver:   driver,
		})
	}

	return aliases
}

// findBestMatch finds the best matching VFIO driver for the given modalias
// by comparing against all available vfio alias patterns. The best match
// is the one with the fewest wildcard characters.
func findBestMatch(deviceModAlias *modAlias, aliases []vfioAlias) string {
	var bestDriver string
	bestWildcardCount := math.MaxInt

	for _, alias := range aliases {
		if matches, wildcardCount := matchModalias(deviceModAlias, alias.modAlias); matches {
			if wildcardCount < bestWildcardCount {
				bestDriver = alias.driver
				bestWildcardCount = wildcardCount
			}
		}
	}

	return bestDriver
}

// matchModalias checks if a device modalias matches a pattern from modules.alias
// Returns true if it matches and the number of wildcards.
func matchModalias(deviceModAlias, patternModAlias *modAlias) (bool, int) {
	wildcardCount := 0

	modAliasType := reflect.TypeOf(*deviceModAlias)
	deviceModAliasValue := reflect.ValueOf(*deviceModAlias)
	patternModAliasValue := reflect.ValueOf(*patternModAlias)

	// iterate over both modAlias structs, comparing each field
	for i := 0; i < modAliasType.NumField(); i++ {
		deviceValue := deviceModAliasValue.Field(i).String()
		patternValue := patternModAliasValue.Field(i).String()

		if patternValue == "*" {
			wildcardCount++
			continue
		}

		if deviceValue != patternValue {
			return false, wildcardCount
		}
	}
	return true, wildcardCount
}

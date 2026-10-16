/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShouldRemoveCliqueLabelOnStop(t *testing.T) {
	assert.True(t, shouldRemoveCliqueLabelOnStop(true, ""))
	assert.False(t, shouldRemoveCliqueLabelOnStop(true, "08a5f01c-b566-45ea-abb9-6522b1bcbc22"))
	assert.False(t, shouldRemoveCliqueLabelOnStop(false, ""))
	assert.False(t, shouldRemoveCliqueLabelOnStop(false, "08a5f01c-b566-45ea-abb9-6522b1bcbc22"))
}

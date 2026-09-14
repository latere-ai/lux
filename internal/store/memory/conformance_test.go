// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory_test

import (
	"testing"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/store/storetest"
)

// TestStoreConformance is spec 010's suite over the memory store.
func TestStoreConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return memory.New() })
}

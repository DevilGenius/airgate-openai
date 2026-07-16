package gateway

import "unsafe"

// readOnlyBytesString returns a zero-allocation string view for read-only JSON
// parsing. The result must not outlive or be retained beyond the source slice.
func readOnlyBytesString(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(value), len(value))
}

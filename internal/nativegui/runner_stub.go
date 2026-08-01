//go:build !nativegui || !linux || !cgo

package nativegui

func runNative(_ uint64) int {
	return 78
}

func wakeNative(_ uint64) bool {
	return false
}

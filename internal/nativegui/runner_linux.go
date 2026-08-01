//go:build nativegui && linux && cgo

package nativegui

/*
#cgo LDFLAGS: -L${SRCDIR}/../../native/gui/target/release -lalt_native_gui -ldl -lgcc_s -lutil -lrt -lpthread -lm -lc

#include <stdint.h>
#include <stddef.h>

int32_t alt_native_gui_run(uint64_t handle);
uint8_t alt_native_gui_wake(uint64_t handle);
*/
import "C"

import "unsafe"

// NativeBuildID is set to the linked Rust archive's SHA-256 by the Makefile.
// Besides making the native source state inspectable in the Go link command,
// the changing -X value invalidates Go's final-link cache whenever that archive
// changes.
var NativeBuildID = "development"

func runNative(handle uint64) int {
	if NativeBuildID == "" {
		return -1
	}
	return int(C.alt_native_gui_run(C.uint64_t(handle)))
}

func wakeNative(handle uint64) bool {
	return C.alt_native_gui_wake(C.uint64_t(handle)) != 0
}

//export alt_gui_host_exchange
func alt_gui_host_exchange(
	handle C.uint64_t,
	request *C.uint8_t,
	requestLength C.size_t,
	response *C.uint8_t,
	responseCapacity C.size_t,
) C.int64_t {
	if request == nil {
		return -1
	}
	requestBytes := unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLength))
	responseBytes := exchangeForHandle(
		uint64(handle),
		requestBytes,
		int(responseCapacity),
	)
	if len(responseBytes) > int(responseCapacity) || response == nil {
		return -C.int64_t(len(responseBytes))
	}
	copy(
		unsafe.Slice((*byte)(unsafe.Pointer(response)), int(responseCapacity)),
		responseBytes,
	)
	return C.int64_t(len(responseBytes))
}

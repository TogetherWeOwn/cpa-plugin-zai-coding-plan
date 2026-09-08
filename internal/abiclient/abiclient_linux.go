//go:build linux && cgo

// Package abiclient drives a compiled CLIProxyAPI plugin through the same
// C ABI the host loader uses (internal/pluginhost loader_unix.go): dlopen,
// resolve cliproxy_plugin_init, exchange the plugin function table, then
// issue method calls with envelope responses.
//
// It exists so tests can replay the host's exact wire behavior against the
// real c-shared library. Test files cannot import "C" themselves, so the
// cgo client lives here and the test imports this package.
package abiclient

/*
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*cliproxy_plugin_init_fn)(const void*, cliproxy_plugin_api*);

static void* test_dlopen(const char* path) {
	return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

static void* test_dlsym(void* handle, const char* name) {
	return dlsym(handle, name);
}

static const char* test_dlerror(void) {
	return dlerror();
}

static int test_dlclose(void* handle) {
	return dlclose(handle);
}

static int test_call_init(void* fn, cliproxy_plugin_api* plugin) {
	// The host passes a real host API struct; the scaffold never dereferences it.
	return ((cliproxy_plugin_init_fn)fn)(NULL, plugin);
}

static int test_call_plugin(cliproxy_plugin_call_fn fn, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return fn(method, request, request_len, response);
}

static void test_free_plugin_buffer(cliproxy_plugin_free_fn fn, void* ptr, size_t len) {
	fn(ptr, len);
}

static void test_shutdown_plugin(cliproxy_plugin_shutdown_fn fn) {
	fn();
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Client is an opened plugin library with its exchanged function table.
type Client struct {
	handle unsafe.Pointer
	api    C.cliproxy_plugin_api
}

// Open loads the library and runs its cliproxy_plugin_init, mirroring the
// host loader: RTLD_NOW|RTLD_LOCAL, symbol lookup, init, then ABI check.
func Open(path string) (*Client, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	handle := C.test_dlopen(cPath)
	if handle == nil {
		return nil, fmt.Errorf("dlopen %s: %s", path, C.GoString(C.test_dlerror()))
	}
	cSymbol := C.CString("cliproxy_plugin_init")
	initSymbol := C.test_dlsym(handle, cSymbol)
	C.free(unsafe.Pointer(cSymbol))
	if initSymbol == nil {
		C.test_dlclose(handle)
		return nil, fmt.Errorf("missing cliproxy_plugin_init: %s", C.GoString(C.test_dlerror()))
	}
	client := &Client{handle: handle}
	if rc := C.test_call_init(initSymbol, &client.api); rc != 0 {
		client.Close()
		return nil, fmt.Errorf("cliproxy_plugin_init returned %d", int(rc))
	}
	if uint32(client.api.abi_version) != pluginabi.ABIVersion {
		client.Close()
		return nil, fmt.Errorf("plugin ABI version = %d, want %d", uint32(client.api.abi_version), pluginabi.ABIVersion)
	}
	if client.api.call == nil || client.api.free_buffer == nil {
		client.Close()
		return nil, fmt.Errorf("plugin function table is incomplete")
	}
	return client, nil
}

// Call invokes a plugin method and returns the raw envelope response. A
// non-zero return code with an error envelope still yields the envelope,
// matching how the host treats plugin errors.
func (c *Client) Call(method string, request []byte) ([]byte, error) {
	if c == nil || c.api.call == nil {
		return nil, fmt.Errorf("plugin client is closed")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cRequest unsafe.Pointer
	if len(request) > 0 {
		cRequest = C.CBytes(request)
		defer C.free(cRequest)
	}
	var response C.cliproxy_buffer
	rc := C.test_call_plugin(c.api.call, cMethod, (*C.uint8_t)(cRequest), C.size_t(len(request)), &response)
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.test_free_plugin_buffer(c.api.free_buffer, response.ptr, response.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("plugin call %s returned %d: %s", method, int(rc), out)
	}
	return out, nil
}

// Close runs the plugin shutdown hook and unloads the library.
func (c *Client) Close() {
	if c == nil {
		return
	}
	if c.api.shutdown != nil {
		C.test_shutdown_plugin(c.api.shutdown)
		c.api.shutdown = nil
	}
	if c.handle != nil {
		C.test_dlclose(c.handle)
		c.handle = nil
	}
}

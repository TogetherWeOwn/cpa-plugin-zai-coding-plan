//go:build linux && cgo

// Package main implements the zai-coding-plan CLIProxyAPI plugin skeleton.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

var runtimeState pluginRuntime

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	_ = host
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	payload := C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	var raw []byte
	var err error
	switch C.GoString(method) {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if request == nil && requestLen > 0 {
			raw = errorEnvelope("invalid_request", "request body is required")
			break
		}
		var requestBytes []byte
		if requestLen > 0 {
			requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
		}
		if errDecode := json.Unmarshal(requestBytes, &lifecycle); errDecode != nil {
			raw = errorEnvelope("invalid_request", "request body is invalid")
			break
		}
		if errConfig := runtimeState.reconfigure(lifecycle.ConfigYAML); errConfig != nil {
			raw = errorEnvelope("invalid_config", errConfig.Error())
			break
		}
		raw, err = okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		raw, err = schedulerPick(payload)
	case pluginabi.MethodUsageHandle:
		raw, err = usageHandle(payload)
	case pluginabi.MethodManagementRegister:
		raw, err = okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		raw, err = managementHandle(payload)
	case pluginabi.MethodPluginShutdown:
		raw, err = okEnvelope(struct{}{})
	default:
		raw = errorEnvelope("unknown_method", "method is not implemented by this plugin")
	}
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	_ = length
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

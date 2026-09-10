// host-integration verifies the built plugin through the pinned CLIProxyAPI image binary.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/abiclient"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

const (
	pluginID   = "zai-coding-plan"
	statusPath = "/v0/management/plugins/zai-coding-plan/status"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("host-integration", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	hostBinary := flags.String("host-binary", "", "CLIProxyAPI binary extracted from the approved image")
	plugin := flags.String("plugin", "", "built plugin shared library")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *hostBinary == "" || *plugin == "" {
		return errors.New("host-binary and plugin are required")
	}
	if err := verifyCapabilities(*plugin); err != nil {
		return err
	}
	return verifyHostHTTP(*hostBinary, *plugin)
}

func verifyCapabilities(plugin string) error {
	client, err := abiclient.Open(plugin)
	if err != nil {
		return fmt.Errorf("load plugin registration: %w", err)
	}
	defer client.Close()
	root, err := os.MkdirTemp("", "zai-capability-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	cpaConfigPath := filepath.Join(root, "config.yaml")
	const planKey = "release-capability-plan-key"
	cpaConfig := fmt.Sprintf(`auth-dir: %q
claude-api-key:
  - api-key: %q
    base-url: https://api.z.ai/api/anthropic
    prefix: zai
openai-compatibility:
  - name: zai-coding-plan
    base-url: https://api.z.ai/api/coding/paas/v4
    api-key-entries:
      - api-key: %q
plugins:
  enabled: true
  dir: plugins
  configs:
    zai-coding-plan:
      enabled: true
      priority: 1000
`, filepath.Join(root, "auth"), planKey, planKey)
	if err := os.WriteFile(cpaConfigPath, []byte(cpaConfig), 0o600); err != nil {
		return err
	}
	pluginConfig := fmt.Sprintf("cpa-config-path: %s\ndefault-plan: pro\naccounts:\n  - key-suffix: plan-key\n    name: release-capability\n    plan: pro\n", cpaConfigPath)
	request, err := json.Marshal(map[string]any{"config_yaml": []byte(pluginConfig)})
	if err != nil {
		return err
	}
	raw, callErr := client.Call(pluginabi.MethodPluginRegister, request)
	if callErr != nil && len(raw) == 0 {
		return fmt.Errorf("plugin registration: %w", callErr)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode registration envelope: %w (raw %q, call error %v)", err, raw, callErr)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("registration returned no result (raw %q, call error %v)", raw, callErr)
	}
	var registration struct {
		Capabilities struct {
			Scheduler     bool `json:"scheduler"`
			UsagePlugin   bool `json:"usage_plugin"`
			ManagementAPI bool `json:"management_api"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		return fmt.Errorf("decode registration result: %w", err)
	}
	caps := registration.Capabilities
	if !envelope.OK || !caps.Scheduler || !caps.UsagePlugin || !caps.ManagementAPI {
		return fmt.Errorf("required capabilities not advertised: scheduler=%t usage_plugin=%t management_api=%t", caps.Scheduler, caps.UsagePlugin, caps.ManagementAPI)
	}
	return nil
}

func verifyHostHTTP(hostBinary, plugin string) error {
	root, err := os.MkdirTemp("", "zai-host-integration-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	pluginDir := filepath.Join(root, "plugins", "linux", "amd64")
	authDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return err
	}
	pluginDst := filepath.Join(pluginDir, pluginID+"-v0.1.0.so")
	if err := copyFile(pluginDst, plugin, 0o755); err != nil {
		return err
	}

	const planKey = "release-integration-plan-key"
	const managementKey = "release-integration-management-key"
	port, err := availablePort()
	if err != nil {
		return err
	}
	configPath := filepath.Join(root, "config.yaml")
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
remote-management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
auth-dir: %q
api-keys: ["release-integration-client-key"]
claude-api-key:
  - api-key: %q
    base-url: https://api.z.ai/api/anthropic
    prefix: zai
openai-compatibility:
  - name: zai-coding-plan
    base-url: https://api.z.ai/api/coding/paas/v4
    api-key-entries:
      - api-key: %q
plugins:
  enabled: true
  dir: %q
  configs:
    zai-coding-plan:
      enabled: true
      priority: 1000
      cpa-config-path: %q
      default-plan: pro
      accounts:
        - key-suffix: plan-key
          name: release-integration
          plan: pro
`, port, managementKey, authDir, planKey, planKey, filepath.Join(root, "plugins"), configPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return err
	}

	logFile, err := os.OpenFile(filepath.Join(root, "host.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	command := exec.Command(hostBinary, "-config", configPath)
	command.Dir = root
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		return fmt.Errorf("start approved host binary: %w", err)
	}
	defer stopProcess(command)

	client := &http.Client{Timeout: 3 * time.Second}
	statusURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, statusPath)
	if err := waitForStatus(client, statusURL, managementKey); err != nil {
		return withHostLog(root, err)
	}
	unauthorized, err := requestStatus(client, statusURL, "")
	if err != nil {
		return withHostLog(root, err)
	}
	if unauthorized.StatusCode != http.StatusUnauthorized {
		return withHostLog(root, fmt.Errorf("unauthenticated status code = %d, want 401", unauthorized.StatusCode))
	}
	authorized, err := requestStatus(client, statusURL, managementKey)
	if err != nil {
		return withHostLog(root, err)
	}
	if authorized.StatusCode != http.StatusOK {
		return withHostLog(root, fmt.Errorf("authenticated status code = %d, want 200", authorized.StatusCode))
	}
	var body struct {
		Plugin   string `json:"plugin"`
		Status   string `json:"status"`
		Accounts []struct {
			KeySuffix string `json:"key_suffix"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(authorized.Body, &body); err != nil {
		return fmt.Errorf("decode authenticated status: %w", err)
	}
	if body.Plugin != pluginID || body.Status != "registered" || len(body.Accounts) != 1 || body.Accounts[0].KeySuffix != "redacted" {
		return fmt.Errorf("authenticated status is not registered and redacted: plugin=%q status=%q accounts=%d suffix=%q", body.Plugin, body.Status, len(body.Accounts), firstSuffix(body.Accounts))
	}
	fmt.Println("approved host image: plugin registered; scheduler, usage_plugin, management_api advertised; status 401/200; account suffix redacted")
	return nil
}

type statusResponse struct {
	StatusCode int
	Body       []byte
}

func availablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve host integration port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForStatus(client *http.Client, url, key string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := requestStatus(client, url, key)
		if err == nil && response.StatusCode == http.StatusOK {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("approved host image did not expose the authenticated plugin status within 30 seconds")
}

func requestStatus(client *http.Client, url, key string) (statusResponse, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return statusResponse{}, err
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := client.Do(request)
	if err != nil {
		return statusResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return statusResponse{}, err
	}
	return statusResponse{StatusCode: response.StatusCode, Body: body}, nil
}

func firstSuffix(accounts []struct {
	KeySuffix string `json:"key_suffix"`
}) string {
	if len(accounts) == 0 {
		return ""
	}
	return accounts[0].KeySuffix
}

func copyFile(dst, src string, mode os.FileMode) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, mode)
}

func stopProcess(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Kill()
	_ = command.Wait()
}

func withHostLog(root string, cause error) error {
	raw, err := os.ReadFile(filepath.Join(root, "host.log"))
	if err != nil {
		return cause
	}
	return fmt.Errorf("%w\nhost log:\n%s", cause, raw)
}

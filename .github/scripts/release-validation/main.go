// release-validation validates release metadata, artifacts, documentation, and source files.
package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	pluginID             = "zai-coding-plan"
	libraryName          = pluginID + ".so"
	hostImageRepository  = "eceasy/cli-proxy-api"
	hostImageTag         = "v7.2.67"
	hostImageAMD64Digest = "sha256:49a249ba0cb867d2e70ef90f23d5fa8b6e2d04bf6c73d9e666e8eee8c353b606"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

const canonicalMITLicense = `MIT License

Copyright (c) 2026 TogetherWeOwn

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const canonicalNotice = `cpa-plugin-zai-coding-plan
Copyright (c) 2026 TogetherWeOwn

This product includes software developed by TogetherWeOwn and third-party software whose licenses are recorded in the Go module dependency metadata.
`

type registry struct {
	Plugins []registryPlugin `json:"plugins"`
}

type registryPlugin struct {
	ID      string          `json:"id"`
	Version string          `json:"version"`
	License string          `json:"license"`
	Release registryRelease `json:"release"`
}

type registryRelease struct {
	Archive   string `json:"archive"`
	Checksums string `json:"checksums"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("release-validation", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "release", "validation mode: release, source, or version")
	root := flags.String("root", "", "repository root; defaults to the current directory")
	tag := flags.String("tag", "", "release tag, including the v prefix")
	version := flags.String("version", "", "release version without the v prefix")
	dist := flags.String("dist", "dist", "release artifact directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *root == "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
		*root = workingDirectory
	}

	switch *mode {
	case "source":
		return validateSource(*root)
	case "release":
		return validateRelease(*root, *dist, *tag, *version)
	case "version":
		resolved, err := versionFromTag(*tag)
		if err != nil {
			return err
		}
		fmt.Println(resolved)
		return nil
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
}

func validateSource(root string) error {
	if err := validateDocumentation(root); err != nil {
		return err
	}
	if err := validateHostImagePin(root); err != nil {
		return err
	}
	if err := validateWorkflowActionPins(root); err != nil {
		return err
	}
	if err := validateReleaseWorkflowBoundary(root); err != nil {
		return err
	}
	files, err := trackedFiles(root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("source validation found no files")
	}
	return nil
}

func validateRelease(root, dist, tag, version string) error {
	if err := validateVersion(tag, version); err != nil {
		return err
	}
	if err := validateDocumentation(root); err != nil {
		return err
	}
	if err := validateRegistry(root, version); err != nil {
		return err
	}
	if err := validateChangelog(root, version); err != nil {
		return err
	}

	distPath := resolvePath(root, dist)
	libraryPath := filepath.Join(distPath, fmt.Sprintf("%s-v%s.so", pluginID, version))
	archivePath := filepath.Join(distPath, fmt.Sprintf("%s_%s_linux_amd64.zip", pluginID, version))
	checksumsPath := filepath.Join(distPath, "checksums.txt")
	if err := requireNonEmpty(libraryPath); err != nil {
		return err
	}
	if err := requireNonEmpty(archivePath); err != nil {
		return err
	}
	if err := validateArchive(libraryPath, archivePath); err != nil {
		return err
	}
	return validateChecksums(checksumsPath, []string{libraryPath, archivePath})
}

func validateVersion(tag, version string) error {
	resolved, err := versionFromTag(tag)
	if err != nil {
		return err
	}
	if resolved != version {
		return fmt.Errorf("tag %q does not match version %q", tag, version)
	}
	return nil
}

func versionFromTag(tag string) (string, error) {
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("tag %q must start with v", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("tag %q is not valid semantic version syntax", tag)
	}
	return version, nil
}

func validateRegistry(root, version string) error {
	path := filepath.Join(root, "registry.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read registry: %w", err)
	}
	var value registry
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("parse registry: %w", err)
	}
	matches := make([]registryPlugin, 0, 1)
	for _, plugin := range value.Plugins {
		if plugin.ID == pluginID {
			matches = append(matches, plugin)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("registry must contain exactly one %q entry, found %d", pluginID, len(matches))
	}
	if matches[0].Version != version {
		return fmt.Errorf("registry version %q does not match release version %q", matches[0].Version, version)
	}
	if matches[0].License != "MIT" {
		return fmt.Errorf("registry license %q does not match LICENSE", matches[0].License)
	}
	wantArchive := fmt.Sprintf("%s_%s_linux_amd64.zip", pluginID, version)
	if matches[0].Release.Archive != wantArchive {
		return fmt.Errorf("registry archive %q does not match release archive %q", matches[0].Release.Archive, wantArchive)
	}
	if matches[0].Release.Checksums != "checksums.txt" {
		return fmt.Errorf("registry checksums %q must be checksums.txt", matches[0].Release.Checksums)
	}
	return nil
}

func validateChangelog(root, version string) error {
	raw, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return fmt.Errorf("read changelog: %w", err)
	}
	text := string(raw)
	headingPattern := regexp.MustCompile(`(?m)^## \[` + regexp.QuoteMeta(version) + `\] - (\d{4}-\d{2}-\d{2})$`)
	if !headingPattern.MatchString(text) {
		return fmt.Errorf("changelog must contain a dated [%s] release heading", version)
	}
	link := fmt.Sprintf("[%s]: https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/releases/tag/v%s", version, version)
	if !strings.Contains(text, link) {
		return fmt.Errorf("changelog is missing the %s release link", version)
	}
	return nil
}

func validateDocumentation(root string) error {
	license, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		return fmt.Errorf("read LICENSE: %w", err)
	}
	if normalizeDocument(string(license)) != normalizeDocument(canonicalMITLicense) {
		return errors.New("LICENSE does not match the canonical MIT license")
	}

	notice, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		return fmt.Errorf("read NOTICE: %w", err)
	}
	if normalizeDocument(string(notice)) != normalizeDocument(canonicalNotice) {
		return errors.New("NOTICE does not match the canonical project notice")
	}
	return nil
}

func normalizeDocument(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func validateHostImagePin(root string) error {
	path := filepath.Join(root, ".github", "release-host-image.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read release host image pin: %w", err)
	}
	var pin struct {
		Repository     string `json:"repository"`
		Tag            string `json:"tag"`
		Platform       string `json:"platform"`
		ManifestDigest string `json:"manifest_digest"`
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		return fmt.Errorf("parse release host image pin: %w", err)
	}
	if pin.Repository != hostImageRepository || pin.Tag != hostImageTag || pin.Platform != "linux/amd64" || pin.ManifestDigest != hostImageAMD64Digest {
		return fmt.Errorf("release host image pin does not match the approved v7.2.67 linux/amd64 manifest")
	}
	return nil
}

func validateWorkflowActionPins(root string) error {
	var paths []string
	for _, extension := range []string{"*.yml", "*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(root, ".github", "workflows", extension))
		if err != nil {
			return fmt.Errorf("list workflows: %w", err)
		}
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		return errors.New("no GitHub workflows found")
	}
	pinned := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read workflow %s: %w", filepath.Base(path), err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return fmt.Errorf("parse workflow %s: %w", filepath.Base(path), err)
		}
		var uses []*yaml.Node
		collectWorkflowUses(&document, &uses)
		for _, node := range uses {
			if node.Kind != yaml.ScalarNode {
				return fmt.Errorf("workflow %s:%d action uses must be a scalar and cannot use YAML aliases", filepath.Base(path), node.Line)
			}
			value := strings.TrimSpace(node.Value)
			if strings.HasPrefix(value, "./") {
				continue
			}
			parts := strings.Split(value, "@")
			if len(parts) != 2 || !pinned.MatchString(parts[1]) {
				return fmt.Errorf("workflow %s:%d action is not pinned to a full commit SHA", filepath.Base(path), node.Line)
			}
		}
	}
	return nil
}

func collectWorkflowUses(node *yaml.Node, uses *[]*yaml.Node) {
	if node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			if key.Kind != yaml.ScalarNode {
				*uses = append(*uses, key)
			} else if key.Value == "uses" {
				*uses = append(*uses, value)
			}
			collectWorkflowUses(value, uses)
		}
		return
	}
	for _, child := range node.Content {
		collectWorkflowUses(child, uses)
	}
}

func validateReleaseWorkflowBoundary(root string) error {
	ciPath := filepath.Join(root, ".github", "workflows", "ci.yml")
	ciRaw, err := os.ReadFile(ciPath)
	if err != nil {
		return fmt.Errorf("read CI workflow: %w", err)
	}
	var ciDocument yaml.Node
	if err := yaml.Unmarshal(ciRaw, &ciDocument); err != nil {
		return fmt.Errorf("parse CI workflow: %w", err)
	}
	ciWorkflow, err := yamlMapping(&ciDocument, "CI workflow")
	if err != nil {
		return err
	}
	ciTrigger, err := yamlMapping(ciWorkflow["on"], "CI workflow trigger")
	if err != nil {
		return err
	}
	ciPush, err := yamlMapping(ciTrigger["push"], "CI push trigger")
	if err != nil {
		return err
	}
	ciBranches, err := yamlStringSequence(ciPush["branches"], "CI push branches")
	if err != nil || len(ciBranches) != 2 || ciBranches[0] != "main" || ciBranches[1] != "release-recovery/v0.1.0" {
		return errors.New("CI push trigger must contain only main and release-recovery/v0.1.0")
	}

	path := filepath.Join(root, ".github", "workflows", "release.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read release workflow: %w", err)
	}
	if err := validateReleaseWorkflowShape(raw); err != nil {
		return err
	}
	var workflow struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Needs       string            `yaml:"needs"`
			Permissions map[string]string `yaml:"permissions"`
			Outputs     map[string]string `yaml:"outputs"`
			Steps       []struct {
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
				With struct {
					Name string `yaml:"name"`
				} `yaml:"with"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		return fmt.Errorf("parse release workflow: %w", err)
	}
	if workflow.Permissions["contents"] != "read" {
		return errors.New("release workflow must default contents permission to read")
	}
	build, exists := workflow.Jobs["build"]
	if !exists {
		return errors.New("release workflow is missing build job")
	}
	if build.Permissions["contents"] == "write" {
		return errors.New("release build job must not have contents write permission")
	}
	if build.Outputs["tag"] != "${{ steps.target.outputs.tag }}" {
		return errors.New("release build job must export the selected release tag")
	}
	publish, exists := workflow.Jobs["publish"]
	if !exists {
		return errors.New("release workflow is missing publish job")
	}
	if publish.Needs != "build" || publish.Permissions["contents"] != "write" {
		return errors.New("release publish job must depend on build and hold contents write permission")
	}
	for name, job := range workflow.Jobs {
		if name != "publish" && job.Permissions["contents"] == "write" {
			return fmt.Errorf("release job %q must not have contents write permission", name)
		}
	}
	const downloadArtifactAction = "actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093"
	const publishCommand = `gh release create "$RAW_TAG" \
  "release-artifacts/zai-coding-plan-v${VERSION}.so" \
  "release-artifacts/zai-coding-plan_${VERSION}_linux_amd64.zip" \
  release-artifacts/checksums.txt \
  --verify-tag --generate-notes`
	var downloaded, published bool
	for _, step := range publish.Steps {
		switch {
		case step.Uses != "":
			if step.Uses != downloadArtifactAction || step.With.Name != "release-artifacts" || downloaded {
				return errors.New("release publish job may only download the staged release artifacts with the approved action")
			}
			downloaded = true
		case step.Run != "":
			if normalizeShellCommand(step.Run) != normalizeShellCommand(publishCommand) || published {
				return errors.New("release publish job must use the canonical publication command")
			}
			if step.Env["GH_TOKEN"] != "${{ github.token }}" || step.Env["GH_REPO"] != "${{ github.repository }}" || step.Env["VERSION"] != "${{ needs.build.outputs.version }}" || step.Env["RAW_TAG"] != "${{ needs.build.outputs.tag }}" {
				return errors.New("release publish job must use the canonical publication environment")
			}
			published = true
		default:
			return errors.New("release publish job contains an inert step")
		}
	}
	if !downloaded || !published || len(publish.Steps) != 2 {
		return errors.New("release publish job must contain exactly the artifact download and canonical publication steps")
	}
	return nil
}

func validateReleaseWorkflowShape(raw []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("parse release workflow structure: %w", err)
	}
	workflow, err := yamlMapping(&document, "release workflow")
	if err != nil {
		return err
	}
	trigger, err := yamlMapping(workflow["on"], "release workflow trigger")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(trigger, "release workflow trigger", "push", "workflow_run"); err != nil {
		return err
	}
	push, err := yamlMapping(trigger["push"], "release push trigger")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(push, "release push trigger", "tags"); err != nil {
		return err
	}
	tags, err := yamlStringSequence(push["tags"], "release tag trigger")
	if err != nil || len(tags) != 1 || tags[0] != "v*" {
		return errors.New("release push trigger must contain only v* tags")
	}
	workflowRun, err := yamlMapping(trigger["workflow_run"], "release recovery trigger")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(workflowRun, "release recovery trigger", "workflows", "types", "branches"); err != nil {
		return err
	}
	workflows, err := yamlStringSequence(workflowRun["workflows"], "release recovery workflows")
	if err != nil || len(workflows) != 1 || workflows[0] != "CI" {
		return errors.New("release recovery trigger must use only the CI workflow")
	}
	types, err := yamlStringSequence(workflowRun["types"], "release recovery event types")
	if err != nil || len(types) != 1 || types[0] != "completed" {
		return errors.New("release recovery trigger must use only completed events")
	}
	branches, err := yamlStringSequence(workflowRun["branches"], "release recovery branches")
	if err != nil || len(branches) != 1 || branches[0] != "release-recovery/v0.1.0" {
		return errors.New("release recovery trigger must use only release-recovery/v0.1.0")
	}
	jobs, err := yamlMapping(workflow["jobs"], "release workflow jobs")
	if err != nil {
		return err
	}
	build, err := yamlMapping(jobs["build"], "release build job")
	if err != nil {
		return err
	}
	const recoveryGuard = "github.event_name == 'push' || (github.event.workflow_run.conclusion == 'success' && github.event.workflow_run.event == 'push' && github.event.workflow_run.head_repository.full_name == github.repository && github.event.workflow_run.head_branch == 'release-recovery/v0.1.0' && github.event.workflow_run.head_sha == github.workflow_sha)"
	if normalizeShellCommand(yamlScalarValue(build["if"])) != recoveryGuard {
		return errors.New("release build job must use the canonical recovery event guard")
	}
	buildSteps := build["steps"]
	if buildSteps == nil || buildSteps.Kind != yaml.SequenceNode || len(buildSteps.Content) == 0 {
		return errors.New("release build job must contain steps")
	}
	checkout, err := yamlMapping(buildSteps.Content[0], "release checkout step")
	if err != nil {
		return err
	}
	checkoutWith, err := yamlStringMap(checkout["with"], "release checkout inputs")
	if err != nil {
		return err
	}
	if checkoutWith["fetch-depth"] != "0" || checkoutWith["persist-credentials"] != "false" || checkoutWith["ref"] != "${{ github.event_name == 'workflow_run' && 'refs/tags/v0.1.0' || github.ref }}" {
		return errors.New("release checkout must select only the immutable recovery tag without credentials")
	}
	publish, err := yamlMapping(jobs["publish"], "release publish job")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(publish, "release publish job", "needs", "runs-on", "permissions", "steps"); err != nil {
		return err
	}
	if yamlScalarValue(publish["runs-on"]) != "ubuntu-24.04" {
		return errors.New("release publish job must use the approved runner")
	}
	permissions, err := yamlStringMap(publish["permissions"], "release publish job permissions")
	if err != nil {
		return err
	}
	if len(permissions) != 1 || permissions["contents"] != "write" {
		return errors.New("release publish job must hold only contents write permission")
	}
	steps := publish["steps"]
	if steps == nil || steps.Kind != yaml.SequenceNode || len(steps.Content) != 2 {
		return errors.New("release publish job must contain exactly the artifact download and canonical publication steps")
	}
	download, err := yamlMapping(steps.Content[0], "release artifact download step")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(download, "release artifact download step", "name", "uses", "with"); err != nil {
		return err
	}
	downloadWith, err := yamlStringMap(download["with"], "release artifact download inputs")
	if err != nil {
		return err
	}
	if len(downloadWith) != 2 || downloadWith["name"] != "release-artifacts" || downloadWith["path"] != "release-artifacts" {
		return errors.New("release artifact download step must use the canonical inputs")
	}
	publication, err := yamlMapping(steps.Content[1], "release publication step")
	if err != nil {
		return err
	}
	if err := requireOnlyYAMLKeys(publication, "release publication step", "name", "env", "run"); err != nil {
		return err
	}
	publicationEnv, err := yamlStringMap(publication["env"], "release publication environment")
	if err != nil {
		return err
	}
	if len(publicationEnv) != 4 {
		return errors.New("release publication environment must contain exactly the canonical variables")
	}
	return nil
}

func yamlMapping(node *yaml.Node, context string) (map[string]*yaml.Node, error) {
	if node == nil {
		return nil, fmt.Errorf("%s is missing", context)
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil, fmt.Errorf("%s is not a single document", context)
		}
		node = node.Content[0]
	}
	if node.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("%s cannot use YAML aliases", context)
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", context)
	}
	result := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" {
			return nil, fmt.Errorf("%s contains a non-string key at line %d", context, key.Line)
		}
		if _, exists := result[key.Value]; exists {
			return nil, fmt.Errorf("%s contains duplicate key %q", context, key.Value)
		}
		result[key.Value] = node.Content[index+1]
	}
	return result, nil
}

func yamlStringSequence(node *yaml.Node, context string) ([]string, error) {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be a sequence", context)
	}
	result := make([]string, 0, len(node.Content))
	for _, value := range node.Content {
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return nil, fmt.Errorf("%s values must be strings", context)
		}
		result = append(result, value.Value)
	}
	return result, nil
}

func yamlStringMap(node *yaml.Node, context string) (map[string]string, error) {
	if node == nil {
		return map[string]string{}, nil
	}
	mapping, err := yamlMapping(node, context)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(mapping))
	for key, value := range mapping {
		if value.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s value %q must be a scalar", context, key)
		}
		result[key] = value.Value
	}
	return result, nil
}

func yamlScalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func requireOnlyYAMLKeys(mapping map[string]*yaml.Node, context string, allowed ...string) error {
	allowlist := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowlist[key] = struct{}{}
	}
	for key := range mapping {
		if _, exists := allowlist[key]; !exists {
			return fmt.Errorf("%s contains unapproved key %q", context, key)
		}
	}
	return nil
}

func normalizeShellCommand(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func trackedFiles(root string) ([]string, error) {
	// Git's path list is the deterministic source of files to scan. It includes
	// tracked and untracked, non-ignored files while excluding generated output.
	command := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	if len(output) == 0 {
		return nil, nil
	}
	parts := strings.Split(string(output[:len(output)-1]), "\x00")
	files := make([]string, 0, len(parts))
	for _, name := range parts {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

func requireNonEmpty(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required artifact %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("required artifact %s is empty or not a regular file", filepath.Base(path))
	}
	return nil
}

func validateArchive(libraryPath, archivePath string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer archive.Close()
	if len(archive.File) != 1 || archive.File[0].Name != libraryName {
		entries := make([]string, 0, len(archive.File))
		for _, entry := range archive.File {
			entries = append(entries, entry.Name)
		}
		return fmt.Errorf("archive entries %q, want exactly %q", entries, libraryName)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		return fmt.Errorf("open archive entry: %w", err)
	}
	defer entry.Close()
	library, err := os.Open(libraryPath)
	if err != nil {
		return fmt.Errorf("open library: %w", err)
	}
	defer library.Close()
	equal, err := readersEqual(library, entry)
	if err != nil {
		return fmt.Errorf("compare archive library: %w", err)
	}
	if !equal {
		return errors.New("archive library does not match versioned shared library")
	}
	return nil
}

func readersEqual(left, right io.Reader) (bool, error) {
	leftHash := sha256.New()
	rightHash := sha256.New()
	if _, err := io.Copy(leftHash, left); err != nil {
		return false, err
	}
	if _, err := io.Copy(rightHash, right); err != nil {
		return false, err
	}
	return string(leftHash.Sum(nil)) == string(rightHash.Sum(nil)), nil
}

func validateChecksums(path string, artifactPaths []string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open checksums: %w", err)
	}
	defer file.Close()

	checksums := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) != 2 || len(parts[0]) != sha256.Size*2 {
			return fmt.Errorf("invalid checksum line %q", line)
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return fmt.Errorf("invalid checksum for %s: %w", parts[1], err)
		}
		name := strings.TrimPrefix(parts[1], "*")
		if filepath.Base(name) != name {
			return fmt.Errorf("checksum name %q must be a base filename", name)
		}
		if _, exists := checksums[name]; exists {
			return fmt.Errorf("duplicate checksum for %s", name)
		}
		checksums[name] = strings.ToLower(parts[0])
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	if len(checksums) != len(artifactPaths) {
		return fmt.Errorf("checksums contains %d entries, want %d", len(checksums), len(artifactPaths))
	}
	for _, artifactPath := range artifactPaths {
		name := filepath.Base(artifactPath)
		want, exists := checksums[name]
		if !exists {
			return fmt.Errorf("checksums missing %s", name)
		}
		got, err := fileSHA256(artifactPath)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for checksum: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("checksum %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The Registry is authoritative for the manifest contract. These checks cover
// the publication requirements that a standalone CLI must reject before pack.
// Conformance is exercised against registry-core's versioned fixtures.
var (
	exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
	packagePart  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	permission   = regexp.MustCompile(`^(storage|network|secrets|filesystem|beam):[a-z0-9._*-]+$`)
	portName     = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*$`)
	mediaType    = regexp.MustCompile(`^[a-z0-9][a-z0-9_.+-]*/[a-z0-9][a-z0-9_.+-]*$`)
	semanticID   = regexp.MustCompile(`^[a-z][a-z0-9._/-]*/v[1-9][0-9]*$`)
	hostCap      = regexp.MustCompile(`^host:[A-Za-z][A-Za-z0-9.]*$`)
)

// CanonicalManifest matches registry-core's stableJson for JSON manifests.
// Go's default encoder HTML-escapes characters which JavaScript JSON.stringify
// leaves literal, changing the immutable manifest checksum.
func CanonicalManifest(manifest map[string]any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return nil, err
	}
	canonical := bytes.TrimSuffix(output.Bytes(), []byte("\n"))
	// encoding/json escapes these two separators even with SetEscapeHTML(false),
	// while JSON.stringify writes them as UTF-8 code points.
	return unescapeJSONSeparators(canonical), nil
}

func unescapeJSONSeparators(data []byte) []byte {
	var result bytes.Buffer
	for i := 0; i < len(data); {
		if i+6 <= len(data) && data[i] == '\\' && data[i+1] == 'u' &&
			(string(data[i+2:i+6]) == "2028" || string(data[i+2:i+6]) == "2029") {
			backslashes := 0
			for j := i - 1; j >= 0 && data[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 == 0 {
				if data[i+5] == '8' {
					result.WriteRune('\u2028')
				} else {
					result.WriteRune('\u2029')
				}
				i += 6
				continue
			}
		}
		result.WriteByte(data[i])
		i++
	}
	return result.Bytes()
}

func ValidateManifest(manifest map[string]any) error {
	name, ok := manifest["name"].(string)
	if !ok {
		return fmt.Errorf("manifest name must use @scope/name")
	}
	scope, short, err := PackageParts(name)
	if err != nil || !packagePart.MatchString(strings.TrimPrefix(scope, "@")) || !packagePart.MatchString(short) {
		return fmt.Errorf("manifest name must use lowercase @scope/name format")
	}
	version, ok := manifest["version"].(string)
	if !ok || !exactVersion.MatchString(version) {
		return fmt.Errorf("manifest version must be an exact semver version")
	}
	apiVersion, _ := manifest["apiVersion"].(string)
	if apiVersion != "workflow-actions/v1" && apiVersion != "workflow-actions/v2" {
		return fmt.Errorf("unsupported manifest apiVersion %q", manifest["apiVersion"])
	}
	runtime, err := object(manifest["runtime"], "runtime")
	if err != nil {
		return err
	}
	placements, err := manifestStrings(runtime["placements"], "runtime.placements", apiVersion == "workflow-actions/v2")
	if err != nil || len(placements) == 0 {
		return fmt.Errorf("manifest runtime.placements must not be empty or invalid")
	}
	if apiVersion == "workflow-actions/v2" {
		if err := validateV2(manifest, runtime, placements); err != nil {
			return err
		}
	}
	if apiVersion == "workflow-actions/v2" {
		effective := placements
		if execution, ok := manifest["execution"].(map[string]any); ok {
			if supported, exists := execution["supportedPlacements"]; exists {
				effective, err = uniqueStrings(supported, "execution.supportedPlacements")
				if err != nil {
					return err
				}
			}
		}
		if !contains(effective, "local-workers") && !contains(effective, "room-members") {
			return fmt.Errorf("manifest must support an implemented Studio or room-member target")
		}
	}
	fields := []string{"permissions"}
	if apiVersion == "workflow-actions/v2" {
		fields = append(fields, "inputPermissions", "outputPermissions")
	}
	for _, field := range fields {
		if raw, exists := manifest[field]; exists {
			values, err := manifestStrings(raw, field, apiVersion == "workflow-actions/v2")
			if err != nil {
				return err
			}
			for _, value := range values {
				if !permission.MatchString(value) {
					return fmt.Errorf("%s contains invalid permission %q", field, value)
				}
			}
		}
	}
	if apiVersion == "workflow-actions/v2" {
		if _, ok := manifest["inputs"].(map[string]any); !ok {
			return fmt.Errorf("manifest inputs must be an object")
		}
		if _, ok := manifest["outputs"].(map[string]any); !ok {
			return fmt.Errorf("manifest outputs must be an object")
		}
	}
	return nil
}

func projectEntrypoint(root string, manifest map[string]any) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "beam-project.json"))
	if os.IsNotExist(err) {
		return configuredEntrypoint(manifest, nil)
	}
	if err != nil {
		return "", fmt.Errorf("read beam-project.json: %w", err)
	}
	return configuredEntrypoint(manifest, data)
}

func configuredEntrypoint(manifest map[string]any, data []byte) (string, error) {
	entrypoint := "dist/index.mjs"
	if manifest["apiVersion"] == "workflow-actions/v1" {
		if value, ok := manifest["entrypoint"].(string); ok && value != "" {
			entrypoint = value
		}
	}
	if data == nil {
		return entrypoint, nil
	}
	var project map[string]any
	if err := json.Unmarshal(data, &project); err != nil {
		return "", fmt.Errorf("parse beam-project.json: %w", err)
	}
	if project == nil {
		return "", fmt.Errorf("beam-project.json must be an object")
	}
	if err := allowed(project, "beam-project.json", "entrypoint", "scripts"); err != nil {
		return "", err
	}
	if raw, exists := project["entrypoint"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("beam-project.json entrypoint must be a nonempty string")
		}
		entrypoint = value
	}
	if raw, exists := project["scripts"]; exists {
		scripts, err := object(raw, "beam-project.json scripts")
		if err != nil {
			return "", err
		}
		if err := allowed(scripts, "beam-project.json scripts", "build"); err != nil {
			return "", err
		}
		if value, exists := scripts["build"]; exists {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return "", fmt.Errorf("beam-project.json scripts.build must be a nonempty string")
			}
		}
	}
	return entrypoint, nil
}

// ArchivedEntrypoint applies the same local project settings as Inspect to a
// package archive. The project file is not part of the Registry manifest.
func ArchivedEntrypoint(manifest map[string]any, project []byte) (string, error) {
	entrypoint, err := configuredEntrypoint(manifest, project)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(filepath.FromSlash(entrypoint))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entrypoint must stay inside the package directory")
	}
	return filepath.ToSlash(clean), nil
}

func validateV2(m, runtime map[string]any, placements []string) error {
	if err := allowed(m, "manifest", "name", "version", "displayName", "description", "author", "apiVersion", "runtime", "execution", "configSchema", "inputs", "outputs", "permissions", "inputPermissions", "outputPermissions", "trustLevel", "catalog", "contracts"); err != nil {
		return err
	}
	if err := allowed(runtime, "runtime", "placements", "defaultPlacement", "minStudioVersion"); err != nil {
		return err
	}
	for _, p := range placements {
		if !oneOf(p, "local-workers", "room-members", "external-workers", "beamcore-public", "custom") {
			return fmt.Errorf("runtime.placements contains an unsupported placement")
		}
	}
	if value, exists := runtime["defaultPlacement"]; exists && !oneOf(value, placements...) {
		return fmt.Errorf("runtime.defaultPlacement must be a declared placement")
	}
	if value, exists := runtime["minStudioVersion"]; exists && !isVersion(value) {
		return fmt.Errorf("runtime.minStudioVersion must be an exact semver version")
	}
	if value, exists := m["configSchema"]; exists {
		if _, err := object(value, "configSchema"); err != nil {
			return err
		}
	}
	execution, err := object(m["execution"], "execution")
	if err != nil {
		return err
	}
	if err := allowed(execution, "execution", "runtime", "isolation", "defaultPlacement", "supportedPlacements", "taskMode", "distribution", "defaultTimeoutSeconds", "capabilityContract", "minRuntimeVersion", "requiredCapabilities", "requiredResources"); err != nil {
		return err
	}
	if execution["runtime"] != "node" || !oneOf(execution["isolation"], "sandboxed-esm", "trusted-node") {
		return fmt.Errorf("execution requires a supported node runtime and isolation")
	}
	if execution["capabilityContract"] != "action-execution/v1" {
		return fmt.Errorf("execution.capabilityContract must be action-execution/v1")
	}
	if value, exists := execution["minRuntimeVersion"]; exists && !isVersion(value) {
		return fmt.Errorf("execution.minRuntimeVersion must be an exact semver version")
	}
	if execution["isolation"] == "trusted-node" && (!strings.HasPrefix(m["name"].(string), "@beam/") || !oneOf(m["trustLevel"], "builtin", "verified")) {
		return fmt.Errorf("trusted Node isolation requires a first-party verified action")
	}
	if !oneOf(execution["taskMode"], "single-worker", "distributed-workers") {
		return fmt.Errorf("execution.taskMode is unsupported")
	}
	if raw, exists := execution["supportedPlacements"]; exists {
		supported, err := uniqueStrings(raw, "execution.supportedPlacements")
		if err != nil || len(supported) == 0 {
			return fmt.Errorf("execution.supportedPlacements must be a nonempty subset of runtime.placements")
		}
		for _, p := range supported {
			if !contains(placements, p) {
				return fmt.Errorf("execution.supportedPlacements must be a nonempty subset of runtime.placements")
			}
		}
		placements = supported
	}
	for _, key := range []string{"defaultPlacement"} {
		if value, exists := runtime[key]; exists && !oneOf(value, placements...) {
			return fmt.Errorf("runtime.defaultPlacement must be supported by execution")
		}
		if value, exists := execution[key]; exists && !oneOf(value, placements...) {
			return fmt.Errorf("execution.defaultPlacement must be a supported placement")
		}
	}
	if value, exists := execution["defaultTimeoutSeconds"]; exists && !positive(value) {
		return fmt.Errorf("execution.defaultTimeoutSeconds must be a positive safe integer")
	}
	inputs, err := validatePorts(m["inputs"], "inputs")
	if err != nil {
		return err
	}
	outputs, err := validatePorts(m["outputs"], "outputs")
	if err != nil {
		return err
	}
	if len(inputs)+len(outputs) == 0 {
		return fmt.Errorf("v2 requires at least one declared artifact port")
	}
	caps, err := uniqueStrings(execution["requiredCapabilities"], "execution.requiredCapabilities")
	if err != nil {
		return err
	}
	if !contains(caps, "action-execution/v1") || !contains(caps, "action-artifact-ports/v1") {
		return fmt.Errorf("v2 requires action-execution/v1 and action-artifact-ports/v1 capabilities")
	}
	for _, cap := range caps {
		if !knownCapability(cap) {
			return fmt.Errorf("Unknown mandatory action capability %q", cap)
		}
	}
	required, err := object(execution["requiredResources"], "execution.requiredResources")
	if err != nil {
		return err
	}
	if err := allowed(required, "execution.requiredResources", "capacitySlots", "leaseSeconds"); err != nil {
		return err
	}
	if required["capacitySlots"] != float64(1) {
		return fmt.Errorf("execution.requiredResources.capacitySlots must be one")
	}
	lease := number(required["leaseSeconds"])
	if lease < 5 || lease > 120 {
		return fmt.Errorf("execution.requiredResources.leaseSeconds must fit the action-execution/v1 lease range")
	}
	contracts, err := object(m["contracts"], "contracts")
	if err != nil {
		return err
	}
	if err := allowed(contracts, "contracts", "resources", "recovery", "computation"); err != nil {
		return err
	}
	resources, err := object(contracts["resources"], "contracts.resources")
	if err != nil {
		return err
	}
	if err := allowed(resources, "contracts.resources", "cpuMillis", "memoryMiB", "timeoutSeconds", "maxArtifactBytes", "maxArtifacts", "maxInputBytes", "maxOutputBytes"); err != nil {
		return err
	}
	for _, field := range []string{"cpuMillis", "memoryMiB", "timeoutSeconds", "maxArtifactBytes", "maxArtifacts", "maxInputBytes", "maxOutputBytes"} {
		if !positive(resources[field]) {
			return fmt.Errorf("contracts.resources.%s must be a positive safe integer", field)
		}
	}
	if number(resources["maxArtifactBytes"]) > 32768 || number(resources["maxArtifacts"]) > 16 || number(resources["maxInputBytes"])+number(resources["maxOutputBytes"]) > 65536 {
		return fmt.Errorf("Resources exceed the bounded action-artifact-ports/v1 transport")
	}
	if value, exists := execution["defaultTimeoutSeconds"]; exists && number(value) > number(resources["timeoutSeconds"]) {
		return fmt.Errorf("execution.defaultTimeoutSeconds exceeds contracts.resources.timeoutSeconds")
	}
	if lease > number(resources["timeoutSeconds"]) {
		return fmt.Errorf("execution.requiredResources.leaseSeconds exceeds contracts.resources.timeoutSeconds")
	}
	recovery, err := object(contracts["recovery"], "contracts.recovery")
	if err != nil {
		return err
	}
	if err := allowed(recovery, "contracts.recovery", "retry", "externalEffects"); err != nil {
		return err
	}
	if !oneOf(recovery["retry"], "never", "idempotent", "requires-idempotency-key") || !oneOf(recovery["externalEffects"], "none", "idempotent", "non-idempotent") {
		return fmt.Errorf("contracts.recovery has an unsupported retry or external effects policy")
	}
	if recovery["externalEffects"] == "non-idempotent" && recovery["retry"] != "never" {
		return fmt.Errorf("Non-idempotent external effects cannot be retried")
	}
	if recovery["retry"] == "requires-idempotency-key" && !contains(caps, "idempotency-key/v1") {
		return fmt.Errorf("Idempotency-key retries require idempotency-key/v1")
	}
	computation, err := object(contracts["computation"], "contracts.computation")
	if err != nil {
		return err
	}
	if err := allowed(computation, "contracts.computation", "semanticId", "partitioning"); err != nil {
		return err
	}
	if id, ok := computation["semanticId"].(string); !ok || !semanticID.MatchString(id) {
		return fmt.Errorf("contracts.computation.semanticId must be a versioned identifier")
	}
	return validatePartitioning(execution, computation, caps, inputs, outputs)
}

func validatePorts(raw any, path string) (map[string]any, error) {
	ports, err := object(raw, path)
	if err != nil {
		return nil, err
	}
	for name, rawPort := range ports {
		if !portName.MatchString(name) {
			return nil, fmt.Errorf("%s.%s has an invalid port name", path, name)
		}
		port, err := object(rawPort, path+"."+name)
		if err != nil {
			return nil, err
		}
		if err := allowed(port, path+"."+name, "type", "cardinality", "format", "required"); err != nil {
			return nil, err
		}
		if port["type"] != "artifact" {
			return nil, fmt.Errorf("%s.%s must use a Studio artifact port", path, name)
		}
		if !oneOf(port["cardinality"], "one", "many") {
			return nil, fmt.Errorf("%s.%s has an unsupported cardinality", path, name)
		}
		if format, ok := port["format"].(string); !ok || !mediaType.MatchString(format) {
			return nil, fmt.Errorf("%s.%s must declare one exact artifact format", path, name)
		}
		if _, ok := port["required"].(bool); !ok {
			return nil, fmt.Errorf("%s.%s.required must be a boolean", path, name)
		}
	}
	return ports, nil
}

func validatePartitioning(execution, computation map[string]any, caps []string, inputs, outputs map[string]any) error {
	raw, exists := computation["partitioning"]
	if !exists {
		if execution["taskMode"] == "distributed-workers" || execution["distribution"] != nil {
			return fmt.Errorf("Distributed execution requires an explicit partitioning contract")
		}
		return nil
	}
	partition, err := object(raw, "contracts.computation.partitioning")
	if err != nil {
		return err
	}
	if err := allowed(partition, "contracts.computation.partitioning", "inputPort", "outputPort", "aggregation", "equivalentToSingleTask"); err != nil {
		return err
	}
	if execution["taskMode"] != "distributed-workers" || !contains(caps, "partitioned-reduce/v1") {
		return fmt.Errorf("Partitioning requires distributed-workers and partitioned-reduce/v1")
	}
	distribution, err := object(execution["distribution"], "execution.distribution")
	if err != nil {
		return err
	}
	if err := allowed(distribution, "execution.distribution", "mode", "inputKey", "outputKey", "preferredItemsPerTask", "maxParallelism"); err != nil {
		return err
	}
	if distribution["mode"] != "partitioned-reduce" || distribution["inputKey"] != partition["inputPort"] || distribution["outputKey"] != partition["outputPort"] {
		return fmt.Errorf("Partition ports must match execution.distribution inputKey and outputKey")
	}
	for _, field := range []string{"preferredItemsPerTask", "maxParallelism"} {
		if value, exists := distribution[field]; exists && !positive(value) {
			return fmt.Errorf("execution.distribution.%s must be a positive safe integer", field)
		}
	}
	input, ok := inputs[fmt.Sprint(partition["inputPort"])].(map[string]any)
	if !ok || input["cardinality"] != "many" || input["required"] != true || outputs[fmt.Sprint(partition["outputPort"])] == nil {
		return fmt.Errorf("Partitioning requires a collection input and a declared output port")
	}
	if !oneOf(partition["aggregation"], "ordered-concatenate", "associative-commutative") || partition["equivalentToSingleTask"] != true {
		return fmt.Errorf("Partitioning requires an aggregation mode and explicit single-task equivalence")
	}
	return nil
}

func object(value any, path string) (map[string]any, error) {
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	return result, nil
}

func allowed(value map[string]any, path string, keys ...string) error {
	for key := range value {
		if !contains(keys, key) {
			return fmt.Errorf("%s contains unsupported field %q", path, key)
		}
	}
	return nil
}

func uniqueStrings(value any, path string) ([]string, error) {
	return manifestStrings(value, path, true)
}

func manifestStrings(value any, path string, unique bool) ([]string, error) {
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", path)
	}
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok || (unique && contains(values, text)) {
			return nil, fmt.Errorf("%s must be an array of unique strings", path)
		}
		values = append(values, text)
	}
	return values, nil
}

func contains[T comparable](values []T, value T) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func oneOf(value any, values ...string) bool {
	text, ok := value.(string)
	return ok && contains(values, text)
}
func isVersion(value any) bool {
	text, ok := value.(string)
	return ok && exactVersion.MatchString(text)
}
func number(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
func positive(value any) bool {
	n := number(value)
	return n >= 1 && n <= 9007199254740991 && n == float64(int64(n))
}

func knownCapability(value string) bool {
	if oneOf(value, "action-execution/v1", "action-artifact-ports/v1", "action-process-ownership/v1", "runtime:node", "partitioned-reduce/v1", "idempotency-key/v1", "isolation:sandboxed-esm", "isolation:trusted-node") || hostCap.MatchString(value) {
		return true
	}
	return strings.HasPrefix(value, "permission:") && permission.MatchString(strings.TrimPrefix(value, "permission:"))
}

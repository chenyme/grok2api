package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

func normalizeResponsesTools(payload map[string]json.RawMessage) (*responsesToolCompatibility, error) {
	compatibility := newResponsesToolCompatibility()
	tools, hasTools, err := decodeOptionalArray(payload["tools"], "tools")
	if err != nil {
		return nil, err
	}
	if hasTools {
		compatibility.visibleTools = cloneJSONArray(tools)
	}
	clientSearch, err := inspectToolSearch(tools)
	if err != nil {
		return nil, err
	}
	if err := compatibility.normalizeClientToolSearchParallel(payload, clientSearch); err != nil {
		return nil, err
	}

	normalizedTools := make([]any, 0, len(tools))
	for index, rawTool := range tools {
		converted, convertErr := compatibility.normalizeTool(rawTool, "", clientSearch, false, fmt.Sprintf("tools[%d]", index))
		if convertErr != nil {
			return nil, convertErr
		}
		normalizedTools = append(normalizedTools, converted...)
	}

	if rawInput := payload["input"]; !isEmptyJSON(rawInput) {
		var input any
		if err := json.Unmarshal(rawInput, &input); err != nil {
			return nil, &responsesRequestError{Message: "input 必须是字符串或数组", Param: "input", Code: "invalid_parameter"}
		}
		if items, ok := input.([]any); ok {
			rewritten, loadedTools, visibleTools, rewriteErr := compatibility.normalizeInputItems(items)
			if rewriteErr != nil {
				return nil, rewriteErr
			}
			normalizedTools = append(normalizedTools, loadedTools...)
			compatibility.visibleTools = append(compatibility.visibleTools, visibleTools...)
			payload["input"] = mustJSON(rewritten)
		}
	}

	if compatibility.clientSearchTool != nil {
		searchTool, searchErr := compatibility.buildClientSearchFunction()
		if searchErr != nil {
			return nil, searchErr
		}
		normalizedTools = append(normalizedTools, searchTool)
	}
	normalizedTools = dedupeNormalizedTools(normalizedTools)
	if len(normalizedTools) > 0 {
		payload["tools"] = mustJSON(normalizedTools)
	} else if hasTools {
		delete(payload, "tools")
		if _, exists := payload["parallel_tool_calls"]; exists {
			delete(payload, "parallel_tool_calls")
			compatibility.addWarning("parallel_tool_calls_without_tools_ignored")
		}
		compatibility.changed = true
	}
	if err := compatibility.normalizeToolChoice(payload, normalizedTools); err != nil {
		return nil, err
	}
	if !compatibility.changed && len(compatibility.functionSchemas) == 0 {
		return nil, nil
	}
	return compatibility, nil
}

// normalizeClientToolSearchParallel 保证搜索函数先独立完成，再由客户端选择并回传工具定义。
func (c *responsesToolCompatibility) normalizeClientToolSearchParallel(payload map[string]json.RawMessage, clientSearch bool) error {
	if !clientSearch {
		return nil
	}
	raw, exists := payload["parallel_tool_calls"]
	if !exists || isEmptyJSON(raw) {
		payload["parallel_tool_calls"] = mustJSON(false)
		c.changed = true
		return nil
	}
	var parallel bool
	if err := json.Unmarshal(raw, &parallel); err != nil {
		return &responsesRequestError{
			Message: "parallel_tool_calls 必须是布尔值",
			Param:   "parallel_tool_calls", Code: "invalid_parameter",
		}
	}
	if parallel {
		payload["parallel_tool_calls"] = mustJSON(false)
		c.changed = true
		c.addWarning("client_tool_search_forced_serial")
	}
	return nil
}

func decodeOptionalArray(raw json.RawMessage, param string) ([]any, bool, error) {
	if isEmptyJSON(raw) {
		return nil, false, nil
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, &responsesRequestError{Message: param + " 必须是数组", Param: param, Code: "invalid_parameter"}
	}
	return values, true, nil
}

func inspectToolSearch(tools []any) (bool, error) {
	clientSearch := false
	serverSearch := false
	for index, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return false, &responsesRequestError{Message: fmt.Sprintf("tools[%d] 必须是对象", index), Param: fmt.Sprintf("tools[%d]", index), Code: "invalid_parameter"}
		}
		if stringField(tool, "type") != "tool_search" {
			continue
		}
		param := fmt.Sprintf("tools[%d]", index)
		execution := strings.ToLower(strings.TrimSpace(stringField(tool, "execution")))
		if execution == "" || execution == "server" {
			if clientSearch {
				return false, &responsesRequestError{Message: "单次请求不能混用 client 与 server tool_search", Param: param + ".execution", Code: "invalid_parameter"}
			}
			serverSearch = true
			continue
		}
		if execution != "client" {
			return false, &responsesRequestError{Message: "tool_search.execution 必须是 client 或 server", Param: param + ".execution", Code: "invalid_parameter"}
		}
		if clientSearch {
			return false, &responsesRequestError{Message: "单次请求只能声明一个客户端 tool_search", Param: param, Code: "invalid_parameter"}
		}
		if serverSearch {
			return false, &responsesRequestError{Message: "单次请求不能混用 client 与 server tool_search", Param: param + ".execution", Code: "invalid_parameter"}
		}
		clientSearch = true
	}
	return clientSearch, nil
}

func (c *responsesToolCompatibility) normalizeTool(raw any, namespace string, clientSearch, force bool, param string) ([]any, error) {
	tool, ok := raw.(map[string]any)
	if !ok {
		return nil, &responsesRequestError{Message: param + " 必须是对象", Param: param, Code: "invalid_parameter"}
	}
	kind := strings.TrimSpace(stringField(tool, "type"))
	switch kind {
	case "function":
		name := strings.TrimSpace(stringField(tool, "name"))
		if name == "" {
			return nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
		}
		deferred, _ := tool["defer_loading"].(bool)
		if deferred && !clientSearch && !force {
			c.changed = true
			c.addWarning("orphan_deferred_tool_loaded")
		}
		if deferred && clientSearch && !force {
			c.changed = true
			if namespace == "" {
				c.deferredSurfaces = append(c.deferredSurfaces, describeDeferredTool(name, stringField(tool, "description")))
			}
			return nil, nil
		}
		converted := cloneJSONObject(tool)
		if parameters, exists := converted["parameters"]; exists {
			normalized, changed, normalizeErr := normalizeBuildFunctionParametersRootForTool(parameters, param+".parameters", name)
			if normalizeErr != nil {
				return nil, normalizeErr
			}
			if changed {
				converted["parameters"] = normalized
				c.changed = true
				c.addWarning("function_parameters_root_normalized")
			}
		}
		identity := responsesToolIdentity{Kind: responsesFunctionTool, Namespace: namespace, Name: name}
		alias := c.functionAlias(identity)
		if alias != name {
			c.changed = true
			if strings.EqualFold(name, "view_image") && namespace == "" {
				c.addWarning("view_image_name_normalized")
			}
		}
		if parameters, exists := tool["parameters"]; exists && schemaContainsInteger(parameters) {
			c.functionSchemas[alias] = cloneJSONValue(parameters)
		}
		converted["name"] = alias
		if namespace != "" || alias != name {
			c.changed = true
		}
		if _, exists := converted["defer_loading"]; exists {
			delete(converted, "defer_loading")
			c.changed = true
		}
		return []any{converted}, nil
	case "namespace":
		name := strings.TrimSpace(stringField(tool, "name"))
		if name == "" {
			return nil, &responsesRequestError{Message: param + ".name 不能为空", Param: param + ".name", Code: "invalid_parameter"}
		}
		children, ok := tool["tools"].([]any)
		if !ok {
			return nil, &responsesRequestError{Message: param + ".tools 必须是数组", Param: param + ".tools", Code: "invalid_parameter"}
		}
		c.changed = true
		c.addWarning("namespace_flattened")
		if clientSearch && !force && namespaceHasDeferredFunctions(children) {
			c.deferredSurfaces = append(c.deferredSurfaces, describeDeferredTool(name, stringField(tool, "description")))
		}
		converted := make([]any, 0, len(children))
		for index, rawChild := range children {
			child, childOK := rawChild.(map[string]any)
			childParam := fmt.Sprintf("%s.tools[%d]", param, index)
			if !childOK {
				return nil, &responsesRequestError{Message: childParam + " 必须是对象", Param: childParam, Code: "invalid_parameter"}
			}
			if stringField(child, "type") != "function" {
				return nil, &responsesRequestError{Message: "namespace.tools 只能包含 function 工具", Param: childParam + ".type", Code: "invalid_parameter"}
			}
			items, err := c.normalizeTool(child, name, clientSearch, force, childParam)
			if err != nil {
				return nil, err
			}
			converted = append(converted, items...)
		}
		return converted, nil
	case "tool_search":
		if force {
			c.changed = true
			c.addWarning("nested_tool_search_ignored")
			return nil, nil
		}
		execution := strings.ToLower(strings.TrimSpace(stringField(tool, "execution")))
		if execution == "" || execution == "server" {
			// Build 上游没有服务端 Tool Search。将已声明的延迟工具提前展开，
			// 比让 Codex 因一个可选优化能力整次失败更符合兼容层语义。
			c.serverSearchEager = true
			c.changed = true
			c.addWarning("server_tool_search_eager_loaded")
			return nil, nil
		}
		c.changed = true
		c.addWarning("client_tool_search_emulated")
		c.clientSearchTool = cloneJSONObject(tool)
		c.clientSearchParam = param
		return nil, nil
	case "custom":
		return c.normalizeCustomTool(tool, namespace, param)
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
		return c.normalizeWebSearchTool(tool, kind, param)
	case "mcp":
		return c.normalizeMCPTool(tool, clientSearch, force, param)
	case "shell":
		return c.normalizeShellTool(tool, param)
	case "local_shell":
		return c.normalizeLegacyLocalShellTool(tool, param)
	case "apply_patch":
		return c.normalizeApplyPatchTool(tool, param)
	case "x_search":
		return c.normalizeXSearchTool(tool, param)
	case "image_generation", "collections_search", "file_search", "code_execution", "code_interpreter":
		return c.normalizeNativeTool(tool, param)
	case "computer_use_preview":
		return nil, unsupportedBuildToolError(kind, param)
	default:
		if kind == "" {
			return nil, &responsesRequestError{Message: param + ".type 不能为空", Param: param + ".type", Code: "invalid_parameter"}
		}
		return nil, unsupportedBuildToolError(kind, param)
	}
}

const maxRootUnionDepth = 32
const maxRootUnionLeaves = 32

type buildFunctionParametersRootContext struct {
	param    string
	toolName string
}

type rootObjectLeafCollector struct {
	doc              map[string]any
	context          buildFunctionParametersRootContext
	leaves           []map[string]any
	rootUnionKeyword string
	requiresDisjoint bool
	changed          bool
}

// NormalizeBuildFunctionParametersRoot rewrites a function parameters schema so
// Grok Build can compile it: the root is an object, or a root union of object
// leaves. Nested oneOf/anyOf at the root are lifted; $ref inside properties
// and recursive property $ref are left alone.
func NormalizeBuildFunctionParametersRoot(value any, param, toolName string) (any, bool, error) {
	return normalizeBuildFunctionParametersRootForTool(value, param, toolName)
}

// normalizeBuildFunctionParametersRoot removes root-level nullability from function schemas
// and lifts nested root unions. Nested nullable fields remain untouched.
func normalizeBuildFunctionParametersRoot(value any, param string) (any, bool, error) {
	return normalizeBuildFunctionParametersRootForTool(value, param, "")
}

func normalizeBuildFunctionParametersRootForTool(value any, param, toolName string) (any, bool, error) {
	schema, ok := value.(map[string]any)
	if !ok {
		return nil, false, invalidBuildFunctionParametersRoot(buildFunctionParametersRootContext{param: param, toolName: toolName})
	}
	doc := cloneJSONObject(schema)
	context := buildFunctionParametersRootContext{param: param, toolName: toolName}
	collector := rootObjectLeafCollector{doc: doc, context: context}
	if err := collector.walk(doc, nil, nil, 0, 0); err != nil {
		return nil, false, err
	}
	if len(collector.leaves) == 0 {
		return nil, false, invalidBuildFunctionParametersRoot(context)
	}
	if collector.requiresDisjoint && !rootObjectLeavesPairwiseDisjoint(collector.leaves, doc) {
		return nil, false, invalidBuildFunctionParametersRoot(context)
	}
	if !collector.changed {
		return doc, false, nil
	}
	var out map[string]any
	if len(collector.leaves) == 1 {
		out = collector.leaves[0]
	} else {
		if collector.rootUnionKeyword == "" {
			return nil, false, invalidBuildFunctionParametersRoot(context)
		}
		branches := make([]any, len(collector.leaves))
		for i, leaf := range collector.leaves {
			branches[i] = leaf
		}
		out = map[string]any{collector.rootUnionKeyword: branches}
	}
	delete(out, "$defs")
	if defs := referencedDefs(out, doc); len(defs) > 0 {
		out["$defs"] = defs
	}
	return out, !reflect.DeepEqual(out, schema), nil
}

func (c *rootObjectLeafCollector) walk(node any, constraints []map[string]any, seen map[string]struct{}, depth, unionDepth int) error {
	if depth > maxRootUnionDepth || len(c.leaves) > maxRootUnionLeaves {
		return invalidBuildFunctionParametersRoot(c.context)
	}
	schema, ok := node.(map[string]any)
	if !ok {
		return invalidBuildFunctionParametersRoot(c.context)
	}
	if ref, ok := schema["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			return invalidBuildFunctionParametersRoot(c.context)
		}
		if _, loop := seen[ref]; loop {
			return invalidBuildFunctionParametersRoot(c.context)
		}
		resolved, ok := resolveLocalSchemaRef(c.doc, ref)
		if !ok {
			return invalidBuildFunctionParametersRoot(c.context)
		}
		sibling, siblingErr := rootSchemaSiblingConstraint(schema, "$ref")
		if siblingErr != nil {
			return invalidBuildFunctionParametersRoot(c.context)
		}
		if len(sibling) > 0 {
			constraints = append(cloneRootConstraints(constraints), sibling)
		}
		next := cloneRefSeen(seen)
		next[ref] = struct{}{}
		c.changed = true
		return c.walk(resolved, constraints, next, depth+1, unionDepth)
	}
	normalized, nullOnly, typeChanged := normalizeRootObjectType(schema)
	if nullOnly {
		c.changed = true
		return nil
	}
	if typeChanged {
		c.changed = true
	}
	keyword, branches, unionErr := rootUnion(normalized)
	if unionErr != nil {
		return invalidBuildFunctionParametersRoot(c.context)
	}
	if keyword != "" {
		if c.rootUnionKeyword == "" {
			c.rootUnionKeyword = keyword
		} else {
			c.changed = true
			if c.rootUnionKeyword == "oneOf" || keyword != c.rootUnionKeyword {
				c.requiresDisjoint = true
			}
		}
		sibling, siblingErr := rootSchemaSiblingConstraint(normalized, keyword)
		if siblingErr != nil {
			return invalidBuildFunctionParametersRoot(c.context)
		}
		if len(sibling) > 0 {
			constraints = append(cloneRootConstraints(constraints), sibling)
		}
		for _, branch := range branches {
			if err := c.walk(branch, constraints, cloneRefSeen(seen), depth+1, unionDepth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if unionDepth == 0 && c.rootUnionKeyword != "" {
		return invalidBuildFunctionParametersRoot(c.context)
	}
	if !isObjectRootSchema(normalized, c.doc, nil) {
		return invalidBuildFunctionParametersRoot(c.context)
	}
	leaf := cloneJSONObject(normalized)
	if leaf["type"] != "object" {
		leaf["type"] = "object"
		c.changed = true
	}
	delete(leaf, "$defs")
	leaf = applyRootConstraints(leaf, constraints)
	c.leaves = append(c.leaves, leaf)
	if len(c.leaves) > maxRootUnionLeaves {
		return invalidBuildFunctionParametersRoot(c.context)
	}
	return nil
}

func rootUnion(schema map[string]any) (string, []any, error) {
	var keyword string
	var branches []any
	for _, candidate := range []string{"anyOf", "oneOf"} {
		raw, exists := schema[candidate]
		if !exists {
			continue
		}
		if keyword != "" {
			return "", nil, fmt.Errorf("multiple root unions")
		}
		parsed, ok := raw.([]any)
		if !ok || len(parsed) == 0 {
			return "", nil, fmt.Errorf("invalid root union")
		}
		keyword, branches = candidate, parsed
	}
	return keyword, branches, nil
}

func normalizeRootObjectType(schema map[string]any) (map[string]any, bool, bool) {
	out := cloneJSONObject(schema)
	rawTypes, isArray := out["type"].([]any)
	if !isArray {
		return out, out["type"] == "null", false
	}
	filtered, removedNull := withoutNullSchemaTypes(rawTypes)
	if len(filtered) == 0 {
		return out, true, removedNull
	}
	for _, value := range filtered {
		if value != "object" {
			return out, false, false
		}
	}
	out["type"] = "object"
	return out, false, true
}

func rootSchemaSiblingConstraint(schema map[string]any, excluded string) (map[string]any, error) {
	constraint := make(map[string]any)
	for key, value := range schema {
		switch key {
		case excluded, "$defs":
			continue
		case "anyOf", "oneOf":
			return nil, fmt.Errorf("multiple root expressions")
		case "type":
			if value == "object" {
				continue
			}
		case "properties":
			if properties, ok := value.(map[string]any); ok && len(properties) == 0 {
				continue
			}
		case "required":
			if required, ok := value.([]any); ok && len(required) == 0 {
				continue
			}
		case "additionalProperties":
			if allowed, ok := value.(bool); ok && allowed {
				continue
			}
		}
		constraint[key] = cloneJSONValue(value)
	}
	return constraint, nil
}

func cloneRootConstraints(constraints []map[string]any) []map[string]any {
	return append([]map[string]any(nil), constraints...)
}

func applyRootConstraints(leaf map[string]any, constraints []map[string]any) map[string]any {
	if len(constraints) == 0 {
		return leaf
	}
	allOf := make([]any, 0, len(constraints))
	for _, constraint := range constraints {
		if len(constraint) > 0 {
			allOf = append(allOf, cloneJSONObject(constraint))
		}
	}
	allOf = append(allOf, leaf)
	return map[string]any{"type": "object", "allOf": allOf}
}

func cloneRefSeen(seen map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(seen))
	for key, value := range seen {
		out[key] = value
	}
	return out
}

func rootObjectLeavesPairwiseDisjoint(leaves []map[string]any, doc map[string]any) bool {
	for i := 0; i < len(leaves); i++ {
		for j := i + 1; j < len(leaves); j++ {
			if !rootObjectSchemasDisjoint(leaves[i], leaves[j], doc) {
				return false
			}
		}
	}
	return true
}

type rootObjectSchemaFacts struct {
	required map[string]struct{}
	values   map[string]map[string]struct{}
}

func rootObjectSchemasDisjoint(left, right map[string]any, doc map[string]any) bool {
	leftFacts := collectRootObjectSchemaFacts(left, doc, nil)
	rightFacts := collectRootObjectSchemaFacts(right, doc, nil)
	for property := range leftFacts.required {
		if _, required := rightFacts.required[property]; !required {
			continue
		}
		leftValues, leftKnown := leftFacts.values[property]
		rightValues, rightKnown := rightFacts.values[property]
		if leftKnown && rightKnown && finiteValueSetsDisjoint(leftValues, rightValues) {
			return true
		}
	}
	return false
}

func collectRootObjectSchemaFacts(schema map[string]any, doc map[string]any, seen map[string]struct{}) rootObjectSchemaFacts {
	facts := rootObjectSchemaFacts{required: map[string]struct{}{}, values: map[string]map[string]struct{}{}}
	collectRootObjectSchemaFactsInto(schema, doc, seen, &facts)
	return facts
}

func collectRootObjectSchemaFactsInto(schema map[string]any, doc map[string]any, seen map[string]struct{}, facts *rootObjectSchemaFacts) {
	if ref, ok := schema["$ref"].(string); ok {
		if _, loop := seen[ref]; !loop {
			if resolved, resolvedOK := resolveLocalSchemaRef(doc, ref); resolvedOK {
				next := cloneRefSeen(seen)
				next[ref] = struct{}{}
				collectRootObjectSchemaFactsInto(resolved, doc, next, facts)
			}
		}
	}
	if required, ok := schema["required"].([]any); ok {
		for _, raw := range required {
			if property, ok := raw.(string); ok {
				facts.required[property] = struct{}{}
			}
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for property, raw := range properties {
			propertySchema, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			values, known := finiteSchemaValues(propertySchema, doc, nil)
			if !known {
				continue
			}
			if existing, exists := facts.values[property]; exists {
				facts.values[property] = intersectFiniteValueSets(existing, values)
			} else {
				facts.values[property] = values
			}
		}
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, raw := range allOf {
			if part, ok := raw.(map[string]any); ok {
				collectRootObjectSchemaFactsInto(part, doc, cloneRefSeen(seen), facts)
			}
		}
	}
}

func finiteSchemaValues(schema map[string]any, doc map[string]any, seen map[string]struct{}) (map[string]struct{}, bool) {
	if ref, ok := schema["$ref"].(string); ok {
		if _, loop := seen[ref]; loop {
			return nil, false
		}
		resolved, resolvedOK := resolveLocalSchemaRef(doc, ref)
		if !resolvedOK {
			return nil, false
		}
		next := cloneRefSeen(seen)
		next[ref] = struct{}{}
		return finiteSchemaValues(resolved, doc, next)
	}
	if value, exists := schema["const"]; exists {
		key, ok := finiteSchemaValueKey(value)
		if !ok {
			return nil, false
		}
		return map[string]struct{}{key: {}}, true
	}
	if values, ok := schema["enum"].([]any); ok {
		result := make(map[string]struct{}, len(values))
		for _, value := range values {
			key, valid := finiteSchemaValueKey(value)
			if !valid {
				return nil, false
			}
			result[key] = struct{}{}
		}
		return result, true
	}
	return nil, false
}

func finiteSchemaValueKey(value any) (string, bool) {
	switch value.(type) {
	case nil, bool, float64, string:
		raw, err := json.Marshal(value)
		return string(raw), err == nil
	default:
		return "", false
	}
}

func finiteValueSetsDisjoint(left, right map[string]struct{}) bool {
	for value := range left {
		if _, exists := right[value]; exists {
			return false
		}
	}
	return true
}

func intersectFiniteValueSets(left, right map[string]struct{}) map[string]struct{} {
	intersection := make(map[string]struct{})
	for value := range left {
		if _, exists := right[value]; exists {
			intersection[value] = struct{}{}
		}
	}
	return intersection
}

func referencedDefs(node any, doc map[string]any) map[string]any {
	rawDefs, _ := doc["$defs"].(map[string]any)
	if len(rawDefs) == 0 {
		return nil
	}
	needed := map[string]struct{}{}
	collectDefRefs(node, needed)
	changed := true
	for changed {
		changed = false
		for name := range needed {
			extra := map[string]struct{}{}
			collectDefRefs(rawDefs[name], extra)
			for key := range extra {
				if _, exists := needed[key]; !exists {
					needed[key] = struct{}{}
					changed = true
				}
			}
		}
	}
	if len(needed) == 0 {
		return nil
	}
	out := make(map[string]any, len(needed))
	for name := range needed {
		if def, ok := rawDefs[name]; ok {
			out[name] = cloneJSONValue(def)
		}
	}
	return out
}

func collectDefRefs(node any, needed map[string]struct{}) {
	switch typed := node.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			if name, nameOK := rootDefinitionName(ref); nameOK {
				needed[name] = struct{}{}
			}
		}
		for _, value := range typed {
			collectDefRefs(value, needed)
		}
	case []any:
		for _, value := range typed {
			collectDefRefs(value, needed)
		}
	}
}

func rootDefinitionName(ref string) (string, bool) {
	tail := strings.TrimPrefix(ref, "#/$defs/")
	if tail == ref || tail == "" {
		return "", false
	}
	encoded := strings.SplitN(tail, "/", 2)[0]
	return strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~"), true
}

func withoutNullSchemaTypes(types []any) ([]any, bool) {
	filtered := make([]any, 0, len(types))
	removed := false
	for _, value := range types {
		if value == "null" {
			removed = true
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered, removed
}

func isNullOnlySchema(value any) bool {
	schema, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if schema["type"] == "null" {
		return true
	}
	types, ok := schema["type"].([]any)
	if !ok || len(types) == 0 {
		return false
	}
	for _, value := range types {
		if value != "null" {
			return false
		}
	}
	return true
}

func isObjectRootSchema(schema, root map[string]any, visited map[string]struct{}) bool {
	rawType, hasType := schema["type"]
	if rawType == "object" {
		return true
	}
	if types, ok := rawType.([]any); ok && len(types) > 0 {
		for _, value := range types {
			if value != "object" {
				return false
			}
		}
		return true
	}
	if hasType {
		return false
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		return true
	}
	ref, ok := schema["$ref"].(string)
	if !ok {
		return false
	}
	if visited == nil {
		visited = make(map[string]struct{})
	}
	if _, seen := visited[ref]; seen {
		return false
	}
	resolved, ok := resolveLocalSchemaRef(root, ref)
	if !ok {
		return false
	}
	visited[ref] = struct{}{}
	return isObjectRootSchema(resolved, root, visited)
}

func resolveLocalSchemaRef(root map[string]any, ref string) (map[string]any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var current any = root
	for _, encoded := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	resolved, ok := current.(map[string]any)
	return resolved, ok
}

func invalidBuildFunctionParametersRoot(context buildFunctionParametersRootContext) error {
	message := context.param + " 顶层必须是非 nullable 的 object schema"
	if context.toolName != "" {
		message = fmt.Sprintf("工具 %s 的 %s 顶层必须是非 nullable 的 object schema", context.toolName, context.param)
	}
	return &responsesRequestError{
		Message: message,
		Param:   context.param,
		Code:    "invalid_parameter",
	}
}

func namespaceHasDeferredFunctions(children []any) bool {
	for _, rawChild := range children {
		child, ok := rawChild.(map[string]any)
		if !ok || stringField(child, "type") != "function" {
			continue
		}
		if deferred, _ := child["defer_loading"].(bool); deferred {
			return true
		}
	}
	return false
}

func describeDeferredTool(name, description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return name
	}
	if len(description) > 240 {
		description = description[:240]
	}
	return name + ": " + description
}

func (c *responsesToolCompatibility) buildClientSearchFunction() (map[string]any, error) {
	identity := responsesToolIdentity{Kind: responsesToolSearch, Name: "tool_search"}
	description := strings.TrimSpace(stringField(c.clientSearchTool, "description"))
	if description == "" {
		description = "Search for tools needed to continue the task."
	}
	if len(c.deferredSurfaces) > 0 {
		description += "\nDeferred tool surfaces available to search:\n- " + strings.Join(c.deferredSurfaces, "\n- ")
	}
	if len(description) > maxToolSearchDescriptionBytes {
		description = description[:maxToolSearchDescriptionBytes]
	}
	parameters, exists := c.clientSearchTool["parameters"]
	if !exists {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
	} else if _, ok := parameters.(map[string]any); !ok {
		return nil, &responsesRequestError{Message: "tool_search.parameters 必须是对象", Param: c.clientSearchParam + ".parameters", Code: "invalid_parameter"}
	}
	return map[string]any{
		"type": "function", "name": c.alias(identity), "description": description,
		"parameters": cloneJSONValue(parameters),
	}, nil
}

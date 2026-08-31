package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

type subscriptionFormat string

const (
	subscriptionFormatURI  subscriptionFormat = "uri"
	subscriptionFormatYAML subscriptionFormat = "yaml"
	subscriptionFormatJSON subscriptionFormat = "json"
)

var supportedClientTypes = map[string]struct{}{
	"json":       {},
	"v2ray-json": {},
	"clash":      {},
	"singbox":    {},
	"mihomo":     {},
	"stash":      {},
}

// parseSubscriptionPath accepts the normal capability URL and Remnawave's
// explicit native-format suffixes. Keeping the suffix avoids relying solely on
// fragile User-Agent recognition in a client or intermediary proxy.
func parseSubscriptionPath(path string) (shortUUID, clientType string, ok bool) {
	if !strings.HasPrefix(path, "/sub/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/sub/"), "/")
	if len(parts) == 0 || parts[0] == "" || !validShortUUID(parts[0]) {
		return "", "", false
	}
	if len(parts) == 1 {
		return parts[0], "", true
	}
	if len(parts) != 2 {
		return "", "", false
	}
	clientType = strings.ToLower(parts[1])
	if _, supported := supportedClientTypes[clientType]; !supported {
		return "", "", false
	}
	return parts[0], clientType, true
}

func validShortUUID(value string) bool {
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func mergeSubscriptions(main, white *subscriptionResponse) ([]byte, error) {
	if main == nil || white == nil {
		return nil, fmt.Errorf("both subscription responses are required")
	}
	mainFormat, err := detectSubscriptionFormat(main)
	if err != nil {
		return nil, fmt.Errorf("detect Main subscription format: %w", err)
	}
	whiteFormat, err := detectSubscriptionFormat(white)
	if err != nil {
		return nil, fmt.Errorf("detect WhiteList subscription format: %w", err)
	}
	if mainFormat != whiteFormat {
		return nil, fmt.Errorf("subscription format mismatch: Main=%s WhiteList=%s", mainFormat, whiteFormat)
	}
	switch mainFormat {
	case subscriptionFormatURI:
		return mergeURILists(main.body, white.body)
	case subscriptionFormatYAML:
		return mergeYAMLSubscriptions(main.body, white.body)
	case subscriptionFormatJSON:
		return mergeJSONSubscriptions(main.body, white.body)
	default:
		return nil, fmt.Errorf("unsupported subscription format %q", mainFormat)
	}
}

func detectSubscriptionFormat(response *subscriptionResponse) (subscriptionFormat, error) {
	contentType := strings.ToLower(response.header.Get("Content-Type"))
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = mediaType
	}
	switch {
	case strings.Contains(contentType, "yaml"), strings.Contains(contentType, "yml"):
		return subscriptionFormatYAML, nil
	case strings.Contains(contentType, "json"):
		return subscriptionFormatJSON, nil
	}

	body := bytes.TrimSpace(response.body)
	if json.Valid(body) {
		return subscriptionFormatJSON, nil
	}
	if bytes.Contains(body, []byte("://")) || isBase64URIList(body) {
		return subscriptionFormatURI, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err == nil && yamlDocumentMapping(&document) != nil {
		return subscriptionFormatYAML, nil
	}
	return "", fmt.Errorf("unknown Content-Type %q and body representation", contentType)
}

func isBase64URIList(body []byte) bool {
	compact := bytes.Map(func(char rune) rune {
		if char == '\r' || char == '\n' || char == ' ' || char == '\t' {
			return -1
		}
		return char
	}, body)
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(string(compact))
		if err == nil && bytes.Contains(decoded, []byte("://")) {
			return true
		}
	}
	return false
}

func mergeURILists(main, white []byte) ([]byte, error) {
	mainLines, err := subscriptionLines(main)
	if err != nil {
		return nil, err
	}
	whiteLines, err := subscriptionLines(white)
	if err != nil {
		return nil, err
	}
	return []byte(base64.StdEncoding.EncodeToString(append(append(mainLines, '\n'), whiteLines...))), nil
}

func subscriptionLines(body []byte) ([]byte, error) {
	body = bytes.TrimSpace(body)
	if bytes.Contains(body, []byte("://")) {
		return body, nil
	}
	compact := bytes.Map(func(char rune) rune {
		if char == '\r' || char == '\n' || char == ' ' || char == '\t' {
			return -1
		}
		return char
	}, body)
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(string(compact))
		if err == nil && bytes.Contains(decoded, []byte("://")) {
			return bytes.TrimSpace(decoded), nil
		}
	}
	return nil, fmt.Errorf("not a Base64 or URI subscription")
}

// mergeYAMLSubscriptions supports the native layouts emitted by Mihomo,
// Clash, and Stash templates. Server entries and group member lists are
// joined. Conflicting technical proxy names are namespaced in the WhiteList
// document, preserving both independently configured servers.
func mergeYAMLSubscriptions(main, white []byte) ([]byte, error) {
	mainDocument, err := decodeYAMLMapping(main)
	if err != nil {
		return nil, fmt.Errorf("decode Main YAML: %w", err)
	}
	whiteDocument, err := decodeYAMLMapping(white)
	if err != nil {
		return nil, fmt.Errorf("decode WhiteList YAML: %w", err)
	}
	if err := renameConflictingYAMLProxies(mainDocument, whiteDocument); err != nil {
		return nil, err
	}
	if err := mergeYAMLMapping(mainDocument, whiteDocument); err != nil {
		return nil, err
	}
	return yaml.Marshal(mainDocument)
}

// Main and WhiteList can use the same server display name. Mihomo requires
// proxy names to be unique, so prefix only conflicting technical proxies and
// update all references in the WhiteList document before merging it.
func renameConflictingYAMLProxies(main, white *yaml.Node) error {
	mainProxies, mainOK := yamlMappingValue(main, "proxies")
	whiteProxies, whiteOK := yamlMappingValue(white, "proxies")
	if !mainOK || !whiteOK {
		return nil
	}
	if mainProxies.Kind != yaml.SequenceNode || whiteProxies.Kind != yaml.SequenceNode {
		return fmt.Errorf("YAML proxies must be sequences")
	}
	mainByName := make(map[string]*yaml.Node, len(mainProxies.Content))
	usedNames := make(map[string]struct{}, len(mainProxies.Content)+len(whiteProxies.Content))
	for _, proxy := range mainProxies.Content {
		name, err := yamlItemName(proxy)
		if err != nil {
			return fmt.Errorf("Main proxy: %w", err)
		}
		mainByName[name] = proxy
		usedNames[name] = struct{}{}
	}
	for _, proxy := range whiteProxies.Content {
		name, err := yamlItemName(proxy)
		if err != nil {
			return fmt.Errorf("WhiteList proxy: %w", err)
		}
		usedNames[name] = struct{}{}
	}
	replacements := make(map[string]string)
	for _, proxy := range whiteProxies.Content {
		name, _ := yamlItemName(proxy)
		mainProxy, collision := mainByName[name]
		if !collision || yamlNodesEqual(mainProxy, proxy) {
			continue
		}
		newName := uniqueTechnicalName("WL "+name, usedNames)
		nameNode, _ := yamlMappingValue(proxy, "name")
		nameNode.Value = newName
		replacements[name] = newName
		usedNames[newName] = struct{}{}
	}
	replaceYAMLReferences(white, replacements, "")
	return nil
}

func replaceYAMLReferences(node *yaml.Node, replacements map[string]string, mappingKey string) {
	if node == nil || len(replacements) == 0 {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			replaceYAMLReferences(child, replacements, mappingKey)
		}
	case yaml.MappingNode:
		for index := 0; index < len(node.Content); index += 2 {
			replaceYAMLReferences(node.Content[index+1], replacements, node.Content[index].Value)
		}
	case yaml.ScalarNode:
		if mappingKey != "name" {
			if replacement, found := replacements[node.Value]; found {
				node.Value = replacement
			}
		}
	}
}

func decodeYAMLMapping(body []byte) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	root := yamlDocumentMapping(&document)
	if root == nil {
		return nil, fmt.Errorf("root must be a YAML mapping")
	}
	return cloneYAMLNode(root), nil
}

func yamlDocumentMapping(document *yaml.Node) *yaml.Node {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	return root
}

func mergeYAMLMapping(main, white *yaml.Node) error {
	for index := 0; index < len(white.Content); index += 2 {
		key := white.Content[index].Value
		whiteValue := white.Content[index+1]
		mainValue, found := yamlMappingValue(main, key)
		if !found {
			main.Content = append(main.Content, cloneYAMLNode(white.Content[index]), cloneYAMLNode(whiteValue))
			continue
		}
		var err error
		switch key {
		case "proxies":
			err = mergeNamedYAMLSequence(mainValue, whiteValue, false)
		case "proxy-groups":
			err = mergeNamedYAMLSequence(mainValue, whiteValue, true)
		case "proxy-providers":
			err = mergeYAMLProviderMappings(mainValue, whiteValue)
		default:
			if !yamlNodesEqual(mainValue, whiteValue) {
				err = fmt.Errorf("YAML section %q differs between Main and WhiteList", key)
			}
		}
		if err != nil {
			return fmt.Errorf("merge YAML %q: %w", key, err)
		}
	}
	return nil
}

func mergeNamedYAMLSequence(main, white *yaml.Node, mergeMembers bool) error {
	if main.Kind != yaml.SequenceNode || white.Kind != yaml.SequenceNode {
		return fmt.Errorf("expected sequences")
	}
	positions := make(map[string]int, len(main.Content))
	for index, item := range main.Content {
		name, err := yamlItemName(item)
		if err != nil {
			return fmt.Errorf("Main item %d: %w", index, err)
		}
		if _, exists := positions[name]; exists {
			return fmt.Errorf("duplicate Main item %q", name)
		}
		positions[name] = index
	}
	for index, item := range white.Content {
		name, err := yamlItemName(item)
		if err != nil {
			return fmt.Errorf("WhiteList item %d: %w", index, err)
		}
		position, exists := positions[name]
		if !exists {
			main.Content = append(main.Content, cloneYAMLNode(item))
			positions[name] = len(main.Content) - 1
			continue
		}
		if !mergeMembers {
			if !yamlNodesEqual(main.Content[position], item) {
				return fmt.Errorf("conflicting item %q", name)
			}
			continue
		}
		if err := mergeYAMLGroup(main.Content[position], item); err != nil {
			return fmt.Errorf("group %q: %w", name, err)
		}
	}
	return nil
}

func mergeYAMLGroup(main, white *yaml.Node) error {
	if main.Kind != yaml.MappingNode || white.Kind != yaml.MappingNode {
		return fmt.Errorf("group must be a mapping")
	}
	for index := 0; index < len(white.Content); index += 2 {
		key := white.Content[index].Value
		whiteValue := white.Content[index+1]
		mainValue, found := yamlMappingValue(main, key)
		if !found {
			main.Content = append(main.Content, cloneYAMLNode(white.Content[index]), cloneYAMLNode(whiteValue))
			continue
		}
		if key == "proxies" || key == "use" {
			if err := mergeYAMLStringSequence(mainValue, whiteValue); err != nil {
				return fmt.Errorf("merge %q: %w", key, err)
			}
			continue
		}
		if !yamlNodesEqual(mainValue, whiteValue) {
			return fmt.Errorf("field %q differs", key)
		}
	}
	return nil
}

func mergeYAMLStringSequence(main, white *yaml.Node) error {
	if main.Kind != yaml.SequenceNode || white.Kind != yaml.SequenceNode {
		return fmt.Errorf("expected string sequences")
	}
	existing := make(map[string]struct{}, len(main.Content))
	for _, item := range main.Content {
		if item.Kind != yaml.ScalarNode {
			return fmt.Errorf("Main member is not a scalar")
		}
		existing[item.Value] = struct{}{}
	}
	for _, item := range white.Content {
		if item.Kind != yaml.ScalarNode {
			return fmt.Errorf("WhiteList member is not a scalar")
		}
		if _, found := existing[item.Value]; !found {
			main.Content = append(main.Content, cloneYAMLNode(item))
			existing[item.Value] = struct{}{}
		}
	}
	return nil
}

func mergeYAMLProviderMappings(main, white *yaml.Node) error {
	if main.Kind != yaml.MappingNode || white.Kind != yaml.MappingNode {
		return fmt.Errorf("expected provider mappings")
	}
	for index := 0; index < len(white.Content); index += 2 {
		key := white.Content[index].Value
		mainValue, found := yamlMappingValue(main, key)
		if !found {
			main.Content = append(main.Content, cloneYAMLNode(white.Content[index]), cloneYAMLNode(white.Content[index+1]))
			continue
		}
		if !yamlNodesEqual(mainValue, white.Content[index+1]) {
			return fmt.Errorf("provider %q differs", key)
		}
	}
	return nil
}

func yamlMappingValue(node *yaml.Node, key string) (*yaml.Node, bool) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, false
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1], true
		}
	}
	return nil, false
}

func yamlItemName(item *yaml.Node) (string, error) {
	name, found := yamlMappingValue(item, "name")
	if !found || name.Kind != yaml.ScalarNode || name.Value == "" {
		return "", fmt.Errorf("missing non-empty name")
	}
	return name.Value, nil
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	clone := *node
	clone.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		clone.Content[index] = cloneYAMLNode(child)
	}
	clone.Alias = nil
	return &clone
}

func yamlNodesEqual(left, right *yaml.Node) bool {
	leftBytes, leftErr := yaml.Marshal(left)
	rightBytes, rightErr := yaml.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

// mergeJSONSubscriptions handles sing-box and Xray JSON subscriptions. The
// top-level outbound/endpoint lists are joined by tag. A selector with the
// same tag receives the union of its member tags. Conflicting technical tags
// in the WhiteList document are namespaced so both independently configured
// servers remain reachable.
func mergeJSONSubscriptions(main, white []byte) ([]byte, error) {
	mainDocument, err := decodeJSONObject(main)
	if err != nil {
		return nil, fmt.Errorf("decode Main JSON: %w", err)
	}
	whiteDocument, err := decodeJSONObject(white)
	if err != nil {
		return nil, fmt.Errorf("decode WhiteList JSON: %w", err)
	}
	if err := renameConflictingJSONTags(mainDocument, whiteDocument); err != nil {
		return nil, err
	}
	for key, whiteValue := range whiteDocument {
		mainValue, found := mainDocument[key]
		if !found {
			mainDocument[key] = whiteValue
			continue
		}
		switch key {
		case "outbounds", "endpoints":
			merged, err := mergeNamedJSONArrays(mainValue, whiteValue)
			if err != nil {
				return nil, fmt.Errorf("merge JSON %q: %w", key, err)
			}
			mainDocument[key] = merged
		default:
			if !jsonValuesEqual(mainValue, whiteValue) {
				return nil, fmt.Errorf("JSON section %q differs between Main and WhiteList", key)
			}
		}
	}
	return json.Marshal(mainDocument)
}

// JSON subscription formats use tags to connect selectors, outbounds and
// endpoints. A Main and WhiteList host with equal display names is still two
// different credentials, therefore a conflicting WhiteList tag is namespaced
// and every reference in its document is changed before the documents merge.
func renameConflictingJSONTags(main, white map[string]json.RawMessage) error {
	replacements := make(map[string]string)
	usedTags := make(map[string]struct{})
	for _, key := range []string{"outbounds", "endpoints"} {
		mainItems, err := decodeJSONArray(main[key])
		if err != nil {
			return fmt.Errorf("decode Main %s: %w", key, err)
		}
		whiteItems, err := decodeJSONArray(white[key])
		if err != nil {
			return fmt.Errorf("decode WhiteList %s: %w", key, err)
		}
		mainByTag := make(map[string]json.RawMessage, len(mainItems))
		for _, item := range mainItems {
			tag, tagErr := jsonItemTag(item)
			if tagErr != nil {
				return fmt.Errorf("Main %s: %w", key, tagErr)
			}
			mainByTag[tag] = item
			usedTags[tag] = struct{}{}
		}
		for _, item := range whiteItems {
			tag, tagErr := jsonItemTag(item)
			if tagErr != nil {
				return fmt.Errorf("WhiteList %s: %w", key, tagErr)
			}
			usedTags[tag] = struct{}{}
			if mainItem, collision := mainByTag[tag]; collision {
				if _, mergeErr := mergeJSONObjectItem(mainItem, item); mergeErr != nil {
					replacements[tag] = uniqueTechnicalName("WL "+tag, usedTags)
					usedTags[replacements[tag]] = struct{}{}
				}
			}
		}
	}
	if len(replacements) == 0 {
		return nil
	}
	var document any
	encoded, err := json.Marshal(white)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		return err
	}
	replaceJSONReferences(document, replacements)
	updated, err := json.Marshal(document)
	if err != nil {
		return err
	}
	return json.Unmarshal(updated, &white)
}

func decodeJSONArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func replaceJSONReferences(value any, replacements map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if text, isText := item.(string); isText {
				if replacement, found := replacements[text]; found {
					typed[key] = replacement
				}
				continue
			}
			replaceJSONReferences(item, replacements)
		}
	case []any:
		for index, item := range typed {
			if text, isText := item.(string); isText {
				if replacement, found := replacements[text]; found {
					typed[index] = replacement
				}
				continue
			}
			replaceJSONReferences(item, replacements)
		}
	}
}

func uniqueTechnicalName(base string, used map[string]struct{}) string {
	if _, exists := used[base]; !exists {
		return base
	}
	for index := 2; ; index++ {
		candidate := fmt.Sprintf("%s %d", base, index)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

func decodeJSONObject(body []byte) (map[string]json.RawMessage, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, fmt.Errorf("root must be an object")
	}
	return document, nil
}

func mergeNamedJSONArrays(main, white json.RawMessage) (json.RawMessage, error) {
	var mainItems []json.RawMessage
	var whiteItems []json.RawMessage
	if err := json.Unmarshal(main, &mainItems); err != nil {
		return nil, fmt.Errorf("Main value must be an array: %w", err)
	}
	if err := json.Unmarshal(white, &whiteItems); err != nil {
		return nil, fmt.Errorf("WhiteList value must be an array: %w", err)
	}
	positions := make(map[string]int, len(mainItems))
	for index, item := range mainItems {
		tag, err := jsonItemTag(item)
		if err != nil {
			return nil, fmt.Errorf("Main item %d: %w", index, err)
		}
		if _, exists := positions[tag]; exists {
			return nil, fmt.Errorf("duplicate Main tag %q", tag)
		}
		positions[tag] = index
	}
	for index, item := range whiteItems {
		tag, err := jsonItemTag(item)
		if err != nil {
			return nil, fmt.Errorf("WhiteList item %d: %w", index, err)
		}
		position, exists := positions[tag]
		if !exists {
			mainItems = append(mainItems, item)
			positions[tag] = len(mainItems) - 1
			continue
		}
		merged, err := mergeJSONObjectItem(mainItems[position], item)
		if err != nil {
			return nil, fmt.Errorf("tag %q: %w", tag, err)
		}
		mainItems[position] = merged
	}
	merged, err := json.Marshal(mainItems)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(merged), nil
}

func jsonItemTag(item json.RawMessage) (string, error) {
	document, err := decodeJSONObject(item)
	if err != nil {
		return "", err
	}
	var tag string
	if err := json.Unmarshal(document["tag"], &tag); err != nil || tag == "" {
		return "", fmt.Errorf("missing non-empty tag")
	}
	return tag, nil
}

func mergeJSONObjectItem(main, white json.RawMessage) (json.RawMessage, error) {
	mainDocument, err := decodeJSONObject(main)
	if err != nil {
		return nil, err
	}
	whiteDocument, err := decodeJSONObject(white)
	if err != nil {
		return nil, err
	}
	for key, whiteValue := range whiteDocument {
		mainValue, found := mainDocument[key]
		if !found {
			mainDocument[key] = whiteValue
			continue
		}
		if key == "outbounds" || key == "use" {
			merged, err := mergeJSONStringArrays(mainValue, whiteValue)
			if err != nil {
				return nil, fmt.Errorf("merge %q: %w", key, err)
			}
			mainDocument[key] = merged
			continue
		}
		if !jsonValuesEqual(mainValue, whiteValue) {
			return nil, fmt.Errorf("field %q differs", key)
		}
	}
	merged, err := json.Marshal(mainDocument)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(merged), nil
}

func mergeJSONStringArrays(main, white json.RawMessage) (json.RawMessage, error) {
	var mainItems []string
	var whiteItems []string
	if err := json.Unmarshal(main, &mainItems); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(white, &whiteItems); err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(mainItems))
	for _, item := range mainItems {
		existing[item] = struct{}{}
	}
	for _, item := range whiteItems {
		if _, found := existing[item]; !found {
			mainItems = append(mainItems, item)
			existing[item] = struct{}{}
		}
	}
	merged, err := json.Marshal(mainItems)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(merged), nil
}

func jsonValuesEqual(left, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	return json.Unmarshal(left, &leftValue) == nil && json.Unmarshal(right, &rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}

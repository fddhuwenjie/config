package config

import (
	"strconv"
	"strings"

	"dario.cat/mergo"
)

// SourceRuntime is the source marker returned by Config.SourceOf for values
// changed at runtime with Set or SetData.
const SourceRuntime = "runtime"

// sourceNode is one node of the provenance shadow tree. Every data value
// (scalar, nil, map or slice) is represented by a tagged node; children, when
// present, are a map[string]any/map[any]any/[]any whose elements are
// *sourceNode themselves. Keeping the tag on every node lets SourceOf answer
// for parent keys even when a deep merge left the parent node untouched.
type sourceNode struct {
	// tag is the source of the node: a file absolute path or SourceRuntime.
	tag string
	// children are the contained nodes for maps and slices; nil for scalar or
	// explicit-nil leaves.
	children any
}

// sourceMergeFlags is the effective subset of the mergo merge options that the
// provenance merge needs to mirror, so the source tree follows the same
// override/append decisions as the data merge.
type sourceMergeFlags struct {
	overwrite   bool
	appendSlice bool
}

func (c *Config) sourceMergeFlags() sourceMergeFlags {
	cfg := &mergo.Config{}
	for _, opt := range c.opts.MergeOptions {
		opt(cfg)
	}
	return sourceMergeFlags{
		overwrite:   cfg.Overwrite,
		appendSlice: cfg.AppendSlice,
	}
}

// tagSources builds a provenance node for a loaded value, every node of which
// is tagged with the given source.
func tagSources(v any, tag string) *sourceNode {
	node := &sourceNode{tag: tag}
	switch tv := v.(type) {
	case map[string]any:
		children := make(map[string]any, len(tv))
		for k, val := range tv {
			children[k] = tagSources(val, tag)
		}
		node.children = children
	case map[any]any:
		children := make(map[any]any, len(tv))
		for k, val := range tv {
			children[k] = tagSources(val, tag)
		}
		node.children = children
	case []any:
		children := make([]any, len(tv))
		for i := range tv {
			children[i] = tagSources(tv[i], tag)
		}
		node.children = children
	case map[string]string:
		// normalize typed containers from runtime Set values
		children := make(map[string]any, len(tv))
		for k := range tv {
			children[k] = &sourceNode{tag: tag}
		}
		node.children = children
	case []string:
		children := make([]any, len(tv))
		for i := range tv {
			children[i] = &sourceNode{tag: tag}
		}
		node.children = children
	}
	return node
}

// tagSourceRoot builds the provenance root for a config data root, the root
// registry itself stays a plain map of tagged nodes.
func tagSourceRoot(data map[string]any, tag string) map[string]any {
	root := make(map[string]any, len(data))
	for k, v := range data {
		root[k] = tagSources(v, tag)
	}
	return root
}

// mergeSourceRoot merges a newly tagged root into the current provenance root,
// mirroring the mergo decisions made for the matching data merge.
func mergeSourceRoot(dst, src map[string]any, flags sourceMergeFlags) {
	mergeStringNodeMap(dst, src, flags)
}

// mergeNodeTrees merges the src node into the dst node and returns the node to
// keep:
//   - two maps are deep merged key by key, overriding one sub-key does not
//     touch the sibling keys, and the dst node keeps its own tag, since the
//     node itself was not replaced;
//   - non-empty slices replace (or append to) the dst slice;
//   - other nodes follow the overwrite option.
//
// The dst node's children maps may be modified in place.
func mergeNodeTrees(dst, src *sourceNode, flags sourceMergeFlags) *sourceNode {
	if dm, ok := dst.children.(map[string]any); ok {
		if sm, ok := src.children.(map[string]any); ok {
			mergeStringNodeMap(dm, sm, flags)
			return dst
		}
		if flags.overwrite {
			return src
		}
		return dst
	}
	if dam, ok := dst.children.(map[any]any); ok {
		if sam, ok := src.children.(map[any]any); ok {
			for k, sv := range sam {
				sn := sv.(*sourceNode)
				if dv, exists := dam[k]; exists {
					dam[k] = mergeNodeTrees(dv.(*sourceNode), sn, flags)
				} else {
					dam[k] = sn
				}
			}
			return dst
		}
		if flags.overwrite {
			return src
		}
		return dst
	}

	if ds, ok := dst.children.([]any); ok {
		if ss, ok := src.children.([]any); ok {
			// an empty src slice does not override a non-empty dst slice,
			// matching mergo's default behavior.
			if len(ss) == 0 {
				return dst
			}
			if flags.appendSlice {
				dst.children = append(ds, ss...)
				return dst
			}
			if flags.overwrite || len(ds) == 0 {
				return src
			}
			return dst
		}
		if flags.overwrite {
			return src
		}
		return dst
	}

	// scalar or explicit-nil leaves, or mismatched kinds
	if flags.overwrite {
		return src
	}
	return dst
}

// mergeStringNodeMap merges two string-keyed node maps in place.
func mergeStringNodeMap(dm, sm map[string]any, flags sourceMergeFlags) {
	for k, sv := range sm {
		sn := sv.(*sourceNode)
		if dv, exists := dm[k]; exists {
			dm[k] = mergeNodeTrees(dv.(*sourceNode), sn, flags)
		} else {
			dm[k] = sn
		}
	}
}

// deepCopyData clones the mutable containers of a loaded data tree, so a
// staged merge can be discarded on error without touching the published data.
func deepCopyData(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		nm := make(map[string]any, len(tv))
		for k, val := range tv {
			nm[k] = deepCopyData(val)
		}
		return nm
	case map[any]any:
		nm := make(map[any]any, len(tv))
		for k, val := range tv {
			nm[k] = deepCopyData(val)
		}
		return nm
	case []any:
		ns := make([]any, len(tv))
		for i := range tv {
			ns[i] = deepCopyData(tv[i])
		}
		return ns
	case []string:
		return append([]string(nil), tv...)
	case map[string]string:
		nm := make(map[string]string, len(tv))
		for k, val := range tv {
			nm[k] = val
		}
		return nm
	default:
		return v
	}
}

// deepCopySource clones a provenance tree. Nodes themselves are immutable,
// the container children maps/slices are copied.
func deepCopySource(v any) any {
	switch tv := v.(type) {
	case *sourceNode:
		cp := &sourceNode{tag: tv.tag}
		if tv.children != nil {
			cp.children = deepCopySource(tv.children)
		}
		return cp
	case map[string]any:
		nm := make(map[string]any, len(tv))
		for k, val := range tv {
			nm[k] = deepCopySource(val)
		}
		return nm
	case map[any]any:
		nm := make(map[any]any, len(tv))
		for k, val := range tv {
			nm[k] = deepCopySource(val)
		}
		return nm
	case []any:
		ns := make([]any, len(tv))
		for i := range tv {
			ns[i] = deepCopySource(tv[i])
		}
		return ns
	default:
		return v
	}
}

func deepCopyDataMap(data map[string]any) map[string]any {
	if len(data) == 0 {
		return make(map[string]any)
	}
	return deepCopyData(data).(map[string]any)
}

func deepCopySourceRoot(sources map[string]any) map[string]any {
	if len(sources) == 0 {
		return make(map[string]any)
	}
	return deepCopySource(sources).(map[string]any)
}

// SourceOf get the source of the current value by the given key string, using
// the same key path rules as GetValue.
//
// Return:
//   - for values loaded from a file, source is the absolute path of the file
//     that supplied the current value;
//   - for values changed at runtime with Set or SetData, source is "runtime";
//   - ok is false (with an empty source) when the key does not exist.
func SourceOf(key string) (string, bool) { return dc.SourceOf(key) }

// SourceOf get the source of the current value by given key string, using the
// same key path rules as GetValue (key path nesting and "arr[0]" indexes).
//
// The returned source is the absolute path of the config file that supplied
// the current value, or SourceRuntime ("runtime") when the value was changed by
// Set/SetData. Values kept after merging still belong to the original file,
// values taken over by a later file belong to the later file, and overriding
// one sub-key does not affect the source of the sibling keys.
//
// Return ("", false) when the key does not exist.
func (c *Config) SourceOf(key string) (source string, ok bool) {
	sep := c.opts.Delimiter
	if key = formatKey(key, string(sep)); key == "" {
		c.addError(ErrKeyIsEmpty)
		return
	}

	// same locking as Config.GetValue()
	if !c.opts.Readonly {
		c.lock.RLock()
		defer c.lock.RUnlock()
	}

	// is top key
	if mark, exists := c.sources[key]; exists {
		if node, ok := mark.(*sourceNode); ok {
			return node.tag, true
		}
		return "", false
	}

	// has sub key? eg. "lang.dir"
	if strings.IndexByte(key, sep) == -1 {
		return "", false
	}

	// find by path, using the same nesting and bracket index syntax
	// (eg. "top.arr[1]") as GetValue.
	if node, found := findSourceNode(c.sources, strings.Split(key, string(sep))); found {
		return node.tag, true
	}
	return "", false
}

// sourceStep is one navigation step of an expanded key path: a map key or a
// slice index.
type sourceStep struct {
	key     string
	isIndex bool
}

// parseSourceSteps expands one key segment into a map-key step followed by its
// trailing slice index steps, eg. "arr[1][2]" -> key "arr", index 1, index 2.
func parseSourceSteps(seg string) []sourceStep {
	name := seg
	var indexes []int
	for strings.HasSuffix(name, "]") {
		p := strings.LastIndexByte(name, '[')
		if p < 1 {
			break
		}
		n, err := strconv.Atoi(name[p+1 : len(name)-1])
		if err != nil || n < 0 {
			break
		}
		indexes = append([]int{n}, indexes...)
		name = name[:p]
	}

	steps := []sourceStep{{key: name}}
	for _, ix := range indexes {
		steps = append(steps, sourceStep{key: strconv.Itoa(ix), isIndex: true})
	}
	return steps
}

// nodeChildByStep navigates one step from a node's children container. A plain
// numeric key also addresses a slice element, mirroring maputil.GetByPathKeys.
func nodeChildByStep(children any, step sourceStep) (*sourceNode, bool) {
	switch ch := children.(type) {
	case map[string]any:
		v, ok := ch[step.key]
		if !ok {
			return nil, false
		}
		node, ok := v.(*sourceNode)
		return node, ok
	case map[any]any:
		v, ok := ch[step.key]
		if !ok {
			return nil, false
		}
		node, ok := v.(*sourceNode)
		return node, ok
	case []any:
		ix, err := strconv.Atoi(step.key)
		if err != nil || ix < 0 || ix >= len(ch) {
			return nil, false
		}
		node, ok := ch[ix].(*sourceNode)
		return node, ok
	}
	return nil, false
}

// findSourceNode walks the provenance root by the given path keys. It first
// tries the literal key segments (a key may really contain brackets), and then
// retries with expanded slice indexes, exactly like maputil.GetByPathKeys.
func findSourceNode(root map[string]any, keys []string) (*sourceNode, bool) {
	// pass 1: literal segments
	var node *sourceNode
	var children any = root
	for i, seg := range keys {
		container := children
		if i > 0 {
			container = node.children
		}
		n, ok := nodeChildByStep(container, sourceStep{key: seg})
		if !ok {
			node = nil
			break
		}
		node = n
	}
	if node != nil {
		return node, true
	}

	// pass 2: expand the trailing slice indexes, eg. "arr[1]"
	hasIndex := false
	var steps []sourceStep
	for _, seg := range keys {
		ss := parseSourceSteps(seg)
		if len(ss) > 1 {
			hasIndex = true
		}
		steps = append(steps, ss...)
	}
	if !hasIndex {
		return nil, false
	}

	children = root
	for i, step := range steps {
		var n *sourceNode
		var ok bool
		if i == 0 {
			n, ok = nodeChildByStep(children, step)
		} else {
			n, ok = nodeChildByStep(node.children, step)
		}
		if !ok {
			return nil, false
		}
		node = n
	}
	return node, true
}

// setSourceByKeys mirrors maputil.SetByKeys on the provenance tree: it creates
// missing intermediate map nodes and assigns the mark at the path. It returns
// an error when the path runs through a value that cannot have sub-keys, the
// same situation maputil.SetByKeys rejects on the data tree.
func setSourceByKeys(root *map[string]any, keys []string, mark *sourceNode) error {
	steps := flattenSourceSteps(keys)
	if len(steps) == 0 || steps[0].isIndex {
		return errSourcePath(keys)
	}

	first := steps[0]
	if len(steps) == 1 {
		(*root)[first.key] = mark
		return nil
	}

	if child, ok := (*root)[first.key].(*sourceNode); ok {
		if err := setInSourceNode(child, steps[1:], mark); err != nil {
			return err
		}
		(*root)[first.key] = child
		return nil
	}

	(*root)[first.key] = buildSourceChain(steps[1:], mark)
	return nil
}

// setInSourceNode assigns the mark at steps inside node.children, extending
// slices and creating missing maps. The node is modified in place.
func setInSourceNode(node *sourceNode, steps []sourceStep, mark *sourceNode) error {
	if len(steps) == 0 {
		return nil
	}
	step := steps[0]
	rest := steps[1:]
	last := len(rest) == 0

	switch ch := node.children.(type) {
	case map[string]any:
		if last {
			ch[step.key] = mark
			return nil
		}
		if child, ok := ch[step.key].(*sourceNode); ok {
			if err := setInSourceNode(child, rest, mark); err != nil {
				return err
			}
			ch[step.key] = child
			return nil
		}
		ch[step.key] = buildSourceChain(rest, mark)
		return nil
	case map[any]any:
		if last {
			ch[step.key] = mark
			return nil
		}
		if child, ok := ch[step.key].(*sourceNode); ok {
			if err := setInSourceNode(child, rest, mark); err != nil {
				return err
			}
			ch[step.key] = child
			return nil
		}
		ch[step.key] = buildSourceChain(rest, mark)
		return nil
	case []any:
		ix, err := strconv.Atoi(step.key)
		if err != nil || ix < 0 {
			return errSourcePathSteps(steps)
		}
		if ix >= len(ch) {
			grown := make([]any, ix+1)
			copy(grown, ch)
			for i := len(ch); i < ix; i++ {
				grown[i] = &sourceNode{tag: mark.tag}
			}
			ch = grown
		}
		if last {
			ch[ix] = mark
		} else {
			child, ok := ch[ix].(*sourceNode)
			if !ok {
				child = buildSourceChain(rest, mark)
				ch[ix] = child
				return nil
			}
			if err := setInSourceNode(child, rest, mark); err != nil {
				return err
			}
			ch[ix] = child
		}
		node.children = ch
		return nil
	default:
		// scalar or explicit-nil leaf cannot have sub-keys
		return errSourcePathSteps(steps)
	}
}

func flattenSourceSteps(keys []string) []sourceStep {
	var steps []sourceStep
	for _, seg := range keys {
		steps = append(steps, parseSourceSteps(seg)...)
	}
	return steps
}

// buildSourceChain builds missing intermediate nodes for a path, all tagged
// with the mark's source, ending with the mark itself.
func buildSourceChain(steps []sourceStep, mark *sourceNode) *sourceNode {
	if len(steps) == 0 {
		return mark
	}
	step := steps[0]
	child := buildSourceChain(steps[1:], mark)

	if step.isIndex {
		ix, _ := strconv.Atoi(step.key)
		sl := make([]any, ix+1)
		for i := 0; i < ix; i++ {
			sl[i] = &sourceNode{tag: mark.tag}
		}
		sl[ix] = child
		return &sourceNode{tag: mark.tag, children: sl}
	}
	return &sourceNode{
		tag:      mark.tag,
		children: map[string]any{step.key: child},
	}
}

func errSourcePath(keys []string) error {
	return &sourcePathError{path: strings.Join(keys, ".")}
}

func errSourcePathSteps(steps []sourceStep) error {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = s.key
	}
	return &sourcePathError{path: strings.Join(parts, ".")}
}

// sourcePathError mirrors the maputil.SetByKeys failure for a path that runs
// through a non-container value.
type sourcePathError struct {
	path string
}

func (e *sourcePathError) Error() string {
	return "config: cannot set sub-value by path: " + e.path
}

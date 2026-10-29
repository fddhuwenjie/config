package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempConfig writes content to a new file under a temp dir and returns its
// absolute path.
func writeTempConfig(t *testing.T, dir, name, content string) string {
	t.Helper()

	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	abs, err := filepath.Abs(file)
	if err != nil {
		t.Fatalf("abs path of %s: %v", file, err)
	}
	return abs
}

func assertSource(t *testing.T, c *Config, key, want string) {
	t.Helper()

	got, ok := c.SourceOf(key)
	if !ok {
		t.Fatalf("SourceOf(%q) not found, want %q", key, want)
	}
	if got != want {
		t.Fatalf("SourceOf(%q) = %q, want %q", key, got, want)
	}
}

func assertNoSource(t *testing.T, c *Config, key string) {
	t.Helper()

	if got, ok := c.SourceOf(key); ok {
		t.Fatalf("SourceOf(%q) = %q, true, want empty,false", key, got)
	}
}

func TestSourceOfMultiFiles(t *testing.T) {
	base, err := filepath.Abs("testdata/json_base.json")
	if err != nil {
		t.Fatal(err)
	}
	other, err := filepath.Abs("testdata/json_other.json")
	if err != nil {
		t.Fatal(err)
	}

	c := New("source-files")
	if err := c.LoadFiles("testdata/json_base.json", "testdata/json_other.json"); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	// the loaded files records use absolute paths too
	files := c.LoadedFiles()
	if len(files) != 2 || files[0] != base || files[1] != other {
		t.Fatalf("LoadedFiles() = %v, want [%s %s]", files, base, other)
	}

	// keys only the first file provides stay with it
	assertSource(t, c, "key_in_json", base)
	assertSource(t, c, "lang", base)
	assertSource(t, c, "lang.dir", base)
	assertSource(t, c, "lang.allowed.en", base)
	assertSource(t, c, "map1.key1", base)

	// keys present in both files are taken over by the later one
	assertSource(t, c, "name", other)    // "app" -> "app2"
	assertSource(t, c, "age", other)     // 123 -> 12
	assertSource(t, c, "baseKey", other) // "value" -> "value2"
	assertSource(t, c, "debug", other)

	// nested keys: the overridden leaf belongs to the later file, untouched
	// siblings stay with the first file, the parent node is unchanged too
	assertSource(t, c, "map1", base)
	assertSource(t, c, "map1.key", other)  // "val" -> "val2"
	assertSource(t, c, "map1.key2", other) // "val2" -> "val20"

	// whole slice replaced by the later file
	assertSource(t, c, "arr1", other)
	// note: like GetValue, a key without the delimiter is not a path, so
	// "arr1[0]" is a literal key here and does not exist
	assertNoSource(t, c, "arr1[0]")

	// missing key
	assertNoSource(t, c, "not-exist")
	assertNoSource(t, c, "map1.not-exist")
	assertNoSource(t, c, "")
}

func TestSourceOfSliceIndex(t *testing.T) {
	dir := t.TempDir()
	f1 := writeTempConfig(t, dir, "f1.json", `{"top":{"arr":["a","b","c"]}}`)
	f2 := writeTempConfig(t, dir, "f2.json", `{"top":{"other":1}}`)

	c := New("source-index")
	if err := c.LoadFiles(f1, f2); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	// the bracket syntax follows the same key path rules as GetValue
	if v, ok := c.GetValue("top.arr[0]"); !ok || v != "a" {
		t.Fatalf("GetValue top.arr[0] = %v, %v", v, ok)
	}
	assertSource(t, c, "top.arr[0]", f1)
	assertSource(t, c, "top.arr[2]", f1)
	if _, ok := c.GetValue("top.arr[9]"); ok {
		t.Fatal("top.arr[9] must not exist")
	}
	assertNoSource(t, c, "top.arr[9]")

	// the later file does not touch the slice, its elements stay with f1
	assertSource(t, c, "top.arr", f1)
	assertSource(t, c, "top.other", f2)

	// setting one slice element at runtime only changes that element
	if err := c.Set("top.arr[1]", "changed"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v := c.String("top.arr[1]"); v != "changed" {
		t.Fatalf("top.arr[1] = %q, want changed", v)
	}
	assertSource(t, c, "top.arr[0]", f1)
	assertSource(t, c, "top.arr[1]", SourceRuntime)
	assertSource(t, c, "top.arr[2]", f1)
}

func TestSourceOfSetAndSetData(t *testing.T) {
	base, _ := filepath.Abs("testdata/json_base.json")
	other, _ := filepath.Abs("testdata/json_other.json")

	c := New("source-runtime")
	if err := c.LoadFiles("testdata/json_base.json", "testdata/json_other.json"); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	// set one nested sub-key at runtime
	if err := c.Set("map1.key3", "val3"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	assertSource(t, c, "map1.key3", SourceRuntime)
	// siblings and the parent keep their file provenance
	assertSource(t, c, "map1", base)
	assertSource(t, c, "map1.key", other)
	assertSource(t, c, "map1.key1", base)

	// a runtime scalar overrides a file value
	if err := c.Set("name", "runtime-name"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	assertSource(t, c, "name", SourceRuntime)

	// a runtime container value is runtime on every node
	if err := c.Set("sec", map[string]any{"inner": map[string]any{"deep": "v"}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	assertSource(t, c, "sec", SourceRuntime)
	assertSource(t, c, "sec.inner", SourceRuntime)
	assertSource(t, c, "sec.inner.deep", SourceRuntime)

	// yaml.v2-like data set at runtime stays navigable, and a deep set does
	// not touch sibling keys
	if err := c.Set("yml", map[any]any{"db": map[any]any{"host": "h", "port": 1}}); err != nil {
		t.Fatalf("Set yml: %v", err)
	}
	assertSource(t, c, "yml.db.host", SourceRuntime)
	assertSource(t, c, "yml.db.port", SourceRuntime)
	if err := c.Set("yml.db.user", "u"); err != nil {
		t.Fatalf("Set yml.db.user: %v", err)
	}
	assertSource(t, c, "yml.db.user", SourceRuntime)
	assertSource(t, c, "yml.db.host", SourceRuntime)

	// a literal dotted key and a path key coexist
	if err := c.Set("some.key", "literal", false); err != nil {
		t.Fatalf("Set literal: %v", err)
	}
	assertSource(t, c, "some.key", SourceRuntime)

	// SetData replaces everything, old keys are gone
	c.SetData(map[string]any{
		"only": "data",
		"nested": map[string]any{
			"k": "v",
		},
	})
	assertSource(t, c, "only", SourceRuntime)
	assertSource(t, c, "nested.k", SourceRuntime)
	assertNoSource(t, c, "name")
	assertNoSource(t, c, "map1.key")
}

func TestSourceOfExplicitNull(t *testing.T) {
	dir := t.TempDir()
	f1 := writeTempConfig(t, dir, "f1.json", `{"name":"a","nullable":1}`)
	f2 := writeTempConfig(t, dir, "f2.json", `{"nullable":null}`)

	c := New("source-null")
	if err := c.LoadFiles(f1, f2); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	// the explicit null overrides the previous value, and it still has a source
	if v, ok := c.GetValue("nullable"); !ok || v != nil {
		t.Fatalf("nullable = %v, %v, want nil,true", v, ok)
	}
	assertSource(t, c, "nullable", f2)
	assertSource(t, c, "name", f1)
}

func TestSourceLoadFilesBatchAtomic(t *testing.T) {
	dir := t.TempDir()
	a := writeTempConfig(t, dir, "a.json", `{"name":"a","onlyA":1}`)
	b := writeTempConfig(t, dir, "b.json", `{"name":"b"}`)
	bad := writeTempConfig(t, dir, "bad.json", `{"name": }`)

	var events []string
	c := New("source-batch", WithHookFunc(func(event string, c *Config) {
		events = append(events, event)
	}))

	// a failing batch (bad file is in the middle) publishes nothing
	if err := c.LoadFiles(a, bad, b); err == nil {
		t.Fatal("LoadFiles with an invalid file must fail")
	}
	if !c.IsEmpty() {
		t.Fatalf("data must stay empty after the failed batch: %v", c.Data())
	}
	if len(c.LoadedFiles()) != 0 {
		t.Fatalf("loaded files must stay empty: %v", c.LoadedFiles())
	}
	if len(events) != 0 {
		t.Fatalf("no load event must fire on failure, got %v", events)
	}

	// the files before the bad one must not leave provenance either
	assertNoSource(t, c, "name")
	assertNoSource(t, c, "onlyA")

	// a successful batch publishes the consistent state
	if err := c.LoadFiles(a, b); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	if got := c.String("name"); got != "b" {
		t.Fatalf("name = %q, want b", got)
	}
	assertSource(t, c, "onlyA", a)
	assertSource(t, c, "name", b)
	if len(c.LoadedFiles()) != 2 {
		t.Fatalf("LoadedFiles() = %v", c.LoadedFiles())
	}
	if len(events) != 1 || events[0] != OnLoadData {
		t.Fatalf("events = %v, want one %s", events, OnLoadData)
	}

	// a later failing single-file load keeps the previous successful state
	events = nil
	if err := c.LoadFiles(bad); err == nil {
		t.Fatal("LoadFiles of invalid file must fail")
	}
	if got := c.String("name"); got != "b" {
		t.Fatalf("name = %q after failed load, want b", got)
	}
	assertSource(t, c, "onlyA", a)
	assertSource(t, c, "name", b)
	if len(c.LoadedFiles()) != 2 {
		t.Fatalf("LoadedFiles() = %v, must keep the previous records", c.LoadedFiles())
	}
	if len(events) != 0 {
		t.Fatalf("no load event must fire on failure, got %v", events)
	}

	// a missing file must not be recorded, LoadExists ignores it
	if err := c.LoadExists(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatalf("LoadExists missing: %v", err)
	}
	if len(c.LoadedFiles()) != 2 {
		t.Fatalf("missing file must not be recorded: %v", c.LoadedFiles())
	}
}

func TestSourceReloadFiles(t *testing.T) {
	dir := t.TempDir()
	file := writeTempConfig(t, dir, "config.json", `{
		"name": "v1",
		"oldKey": "x",
		"sub": {"k1": 1, "k2": 2}
	}`)

	var events []string
	c := New("source-reload", WithHookFunc(func(event string, c *Config) {
		events = append(events, event)
	}))

	if err := c.LoadFiles(file); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	assertSource(t, c, "name", file)
	assertSource(t, c, "oldKey", file)
	assertSource(t, c, "sub.k1", file)

	// runtime change is visible before reload
	if err := c.Set("name", "runtime-name"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	assertSource(t, c, "name", SourceRuntime)

	// rewrite the file: keys removed, a key changed, a key added
	if err := os.WriteFile(file, []byte(`{
		"name": "v2",
		"newKey": "y",
		"sub": {"k1": 11}
	}`), 0644); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	events = nil
	if err := c.ReloadFiles(); err != nil {
		t.Fatalf("ReloadFiles: %v", err)
	}
	if len(events) != 1 || events[0] != OnReloadData {
		t.Fatalf("events = %v, want one %s", events, OnReloadData)
	}

	// value, sources and removed keys are all consistent with the files
	if got := c.String("name"); got != "v2" {
		t.Fatalf("name = %q, want v2", got)
	}
	assertSource(t, c, "name", file)
	assertSource(t, c, "newKey", file)
	assertSource(t, c, "sub.k1", file)
	assertNoSource(t, c, "oldKey")
	assertNoSource(t, c, "sub.k2")
	if c.Exists("oldKey") || c.Exists("sub.k2") {
		t.Fatal("removed keys must not exist after reload")
	}
}

func TestSourceReloadFilesFailureKeepsState(t *testing.T) {
	dir := t.TempDir()
	file := writeTempConfig(t, dir, "config.json", `{"name":"v1","keep":1}`)

	var events []string
	c := New("source-reload-fail", WithHookFunc(func(event string, c *Config) {
		events = append(events, event)
	}))

	if err := c.LoadFiles(file); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	if err := c.Set("rt", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// corrupt the loaded file, reload must fail closed
	events = nil
	if err := os.WriteFile(file, []byte(`{"name": }`), 0644); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := c.ReloadFiles(); err == nil {
		t.Fatal("ReloadFiles of invalid file must fail")
	}
	if got := c.String("name"); got != "v1" {
		t.Fatalf("name = %q after failed reload, want v1", got)
	}
	assertSource(t, c, "name", file)
	assertSource(t, c, "keep", file)
	assertSource(t, c, "rt", SourceRuntime)
	if len(events) != 0 {
		t.Fatalf("reload event must not fire on failure, got %v", events)
	}

	// fix the file, reload succeeds and publishes the new state
	if err := os.WriteFile(file, []byte(`{"name":"v2"}`), 0644); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := c.ReloadFiles(); err != nil {
		t.Fatalf("ReloadFiles: %v", err)
	}
	if got := c.String("name"); got != "v2" {
		t.Fatalf("name = %q, want v2", got)
	}
	assertSource(t, c, "name", file)
	assertNoSource(t, c, "keep")
	assertNoSource(t, c, "rt")
	if len(events) != 1 || events[0] != OnReloadData {
		t.Fatalf("events = %v, want one %s", events, OnReloadData)
	}
}

func TestSourceOfNestedSiblings(t *testing.T) {
	dir := t.TempDir()
	f1 := writeTempConfig(t, dir, "f1.json", `{
		"app": {"db": {"host": "h1", "port": 1}}
	}`)
	f2 := writeTempConfig(t, dir, "f2.json", `{
		"app": {"db": {"host": "h2"}, "title": "t"}
	}`)

	c := New("source-siblings")
	if err := c.LoadFiles(f1, f2); err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	if got := c.String("app.db.host"); got != "h2" {
		t.Fatalf("host = %q, want h2", got)
	}
	assertSource(t, c, "app", f1)
	assertSource(t, c, "app.db", f1)
	assertSource(t, c, "app.db.host", f2)
	assertSource(t, c, "app.db.port", f1)
	assertSource(t, c, "app.title", f2)
}

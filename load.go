package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dario.cat/mergo"
	"github.com/gookit/goutil/errorx"
	"github.com/gookit/goutil/fsutil"
	"github.com/gookit/goutil/strutil"
)

// LoadFiles load one or multi files, will fire OnLoadData event
//
// The files of one call are loaded as a single consistent batch: the values,
// their sources and the loaded files records are published together only when
// every file was read and parsed successfully, otherwise the previous state is
// kept and the OnLoadData event is not fired.
//
// Usage:
//
//	config.LoadFiles(file1, file2, ...)
func LoadFiles(sourceFiles ...string) error { return dc.LoadFiles(sourceFiles...) }

// LoadFiles load and parse config files, will fire OnLoadData event
func (c *Config) LoadFiles(sourceFiles ...string) error {
	return c.writeAndHook(OnLoadData, func() error {
		return c.stageLoadedFiles(sourceFiles, nil, false)
	})
}

// LoadExists load one or multi files, will ignore not exist
//
// Usage:
//
//	config.LoadExists(file1, file2, ...)
func LoadExists(sourceFiles ...string) error { return dc.LoadExists(sourceFiles...) }

// LoadExists load and parse config files, but will ignore not exists file.
//
// As LoadFiles, the existing files are loaded as a single consistent batch.
func (c *Config) LoadExists(sourceFiles ...string) error {
	return c.writeAndHook(OnLoadData, func() error {
		files := make([]string, 0, len(sourceFiles))
		for _, file := range sourceFiles {
			if file == "" {
				continue
			}
			files = append(files, file)
		}
		return c.stageLoadedFiles(files, nil, true)
	})
}

// FileFilterFn for check file is should load.
type FileFilterFn func(file string, c *Config) (shouldLoad bool, format string)

// LoadFilesByFilter load one or multi files by give filter checked, will fire OnLoadData event
func LoadFilesByFilter(configFiles []string, filter FileFilterFn) error {
	return dc.LoadFilesByFilter(configFiles, filter)
}

// LoadFilesByFilter load one or multi files by give filter checked, will fire OnLoadData event
//   - `filter` return format can be emtpy, will auto detect it by file extension
func (c *Config) LoadFilesByFilter(configFiles []string, filter FileFilterFn) error {
	// tip: run the filter before the data lock is held, it's a user callback and may
	// call the getter methods.
	loadFiles := make([]string, 0, len(configFiles))
	formats := make([]string, 0, len(configFiles))
	for _, file := range configFiles {
		shouldLoad, format := filter(file, c)
		if !shouldLoad {
			continue
		}

		loadFiles = append(loadFiles, file)
		formats = append(formats, format)
	}

	if len(loadFiles) == 0 {
		return nil
	}

	return c.writeAndHook(OnLoadData, func() error {
		return c.stageLoadedFiles(loadFiles, formats, false)
	})
}

// LoadRemote load config data from remote URL.
func LoadRemote(format, url string) error { return dc.LoadRemote(format, url) }

// LoadRemote load config data from remote URL.
//
// Usage:
//
//	c.LoadRemote(config.JSON, "http://abc.com/api-config.json")
func (c *Config) LoadRemote(format, url string) (err error) {
	// create http client
	client := http.Client{Timeout: 300 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}

	//noinspection GoUnhandledErrorResult
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("fetch remote config error, reply status code is %d", resp.StatusCode)
	}

	// read response content. tip: the request is not run under the data lock
	bts, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	return c.writeAndHook(OnLoadData, func() error {
		// the remote URL is the source of its values, see Config.SourceOf
		if err := c.parseSourceCode(format, bts, url); err != nil {
			return err
		}
		c.loadedUrls = append(c.loadedUrls, url)
		return nil
	})
}

// LoadOSEnv load data from OS ENV
//
// Deprecated: please use LoadOSEnvs()
func LoadOSEnv(keys []string, keyToLower bool) { dc.LoadOSEnv(keys, keyToLower) }

// LoadOSEnv load data from os ENV
//
// Deprecated: please use Config.LoadOSEnvs()
func (c *Config) LoadOSEnv(keys []string, keyToLower bool) {
	for _, key := range keys {
		// NOTICE: if is Windows os, os.Getenv() Key is not case-sensitive
		val := os.Getenv(key)
		if keyToLower {
			key = strings.ToLower(key)
		}
		_ = c.Set(key, val)
	}
	c.fireHookScoped(OnLoadData)
}

// LoadOSEnvs load data from OS ENVs. see Config.LoadOSEnvs
func LoadOSEnvs(nameToKeyMap map[string]string) { dc.LoadOSEnvs(nameToKeyMap) }

// LoadOSEnvs load data from os ENVs. format: `{ENV_NAME: config_key}`
//
//   - `config_key` allow use key path. eg: `{"DB_USERNAME": "db.username"}`
func (c *Config) LoadOSEnvs(nameToKeyMap map[string]string) {
	for name, cfgKey := range nameToKeyMap {
		if val := os.Getenv(name); val != "" {
			if cfgKey == "" {
				cfgKey = strings.ToLower(name)
			}
			_ = c.Set(cfgKey, val)
		}
	}

	c.fireHookScoped(OnLoadData)
}

// LoadOSEnvByFilter load OS ENVs by custom fitler func. eg: use for load ENV by prefix.
func LoadOSEnvByFilter(filterFn func(key string) (loadIt bool, cfgKey string)) {
	dc.LoadOSEnvByFilter(filterFn)
}

// LoadOSEnvByFilter load OS ENVs by custom fitler func. eg: use for load ENV by prefix.
//
//   - `filterFn` return cfgKey can be empty, will use key instead.
func (c *Config) LoadOSEnvByFilter(filterFn func(key string) (loadIt bool, cfgKey string)) {
	for _, str := range os.Environ() {
		key, val := strutil.SplitKV(str, "=")
		if loadIt, cfgKey := filterFn(key); loadIt {
			if cfgKey == "" {
				cfgKey = strings.ToLower(key)
			}

			_ = c.Set(cfgKey, val)
		}
	}

	c.fireHookScoped(OnLoadData)
}

// support bound types for CLI flags vars
var validTypes = map[string]int{
	"int":  1,
	"uint": 1,
	"bool": 1,
	// string is default
	"string": 1,
}

// LoadFlags load data from cli flags. see Config.LoadFlags
func LoadFlags(defines []string) error { return dc.LoadFlags(defines) }

// LoadFlags parse command line arguments, based on provide keys.
//
// Usage:
//
//	// 'debug' flag is bool type
//	c.LoadFlags([]string{"env", "debug:bool"})
//	// can with flag desc message
//	c.LoadFlags([]string{"env:set the run env"})
//	c.LoadFlags([]string{"debug:bool:set debug mode"})
//	// can set value to map key. eg: myapp --map1.sub-key=val
//	c.LoadFlags([]string{"--map1.sub-key"})
func (c *Config) LoadFlags(defines []string) (err error) {
	hash := map[string]int8{}

	// bind vars
	for _, str := range defines {
		key, typ, desc := parseVarNameAndType(str)
		if desc == "" {
			desc = "config flag " + key
		}

		switch typ {
		case "int":
			ptr := new(int)
			flag.IntVar(ptr, key, c.Int(key), desc)
			hash[key] = 0
		case "uint":
			ptr := new(uint)
			flag.UintVar(ptr, key, c.Uint(key), desc)
			hash[key] = 0
		case "bool":
			ptr := new(bool)
			flag.BoolVar(ptr, key, c.Bool(key), desc)
			hash[key] = 0
		default: // as string
			ptr := new(string)
			flag.StringVar(ptr, key, c.String(key), desc)
			hash[key] = 0
		}
	}

	// parse and collect
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		name := f.Name
		// only get name in the keys.
		if _, ok := hash[name]; !ok {
			return
		}

		// if f.Value implement the flag.Getter, read typed value
		if gtr, ok := f.Value.(flag.Getter); ok {
			_ = c.Set(name, gtr.Get())
			// } else { // TIP: basic type flag always implements Getter interface
			// 	_ = c.Set(name, f.Value.String()) // ignore error
		}
	})

	c.fireHookScoped(OnLoadData)
	return
}

// LoadData load one or multi data
func LoadData(dataSource ...any) error { return dc.LoadData(dataSource...) }

// LoadData load data from map OR struct
//
// The dataSources type allow:
//   - map[string]any
//   - map[string]string
func (c *Config) LoadData(dataSources ...any) (err error) {
	if c.opts.Delimiter == 0 {
		c.opts.Delimiter = defaultDelimiter
	}

	return c.writeAndHook(OnLoadData, func() error {
		for _, ds := range dataSources {
			if smp, ok := ds.(map[string]string); ok {
				c.loadSMap(smp)
				continue
			}

			if err := mergo.Merge(&c.data, ds, c.opts.MergeOptions...); err != nil {
				return errorx.WithStack(err)
			}

			// data provided programmatically is owned by the runtime
			if dm, ok := ds.(map[string]any); ok {
				mergeSourceRoot(c.sources, tagSourceRoot(dm, SourceRuntime), c.sourceMergeFlags())
			}
		}
		return nil
	})
}

// LoadSMap to config
func (c *Config) LoadSMap(smp map[string]string) {
	_ = c.writeAndHook(OnLoadData, func() error {
		c.loadSMap(smp)
		return nil
	})
}

// loadSMap set the map data to config, the caller must hold the data lock.
func (c *Config) loadSMap(smp map[string]string) {
	for k, v := range smp {
		c.data[k] = v
		c.sources[k] = &sourceNode{tag: SourceRuntime}
	}
}

// LoadSources load one or multi byte data
func LoadSources(format string, src []byte, more ...[]byte) error {
	return dc.LoadSources(format, src, more...)
}

// LoadSources load data from byte content.
//
// Usage:
//
//	config.LoadSources(config.Yaml, []byte(`
//	  name: blog
//	  arr:
//		key: val
//
// `))
func (c *Config) LoadSources(format string, src []byte, more ...[]byte) (err error) {
	return c.writeAndHook(OnLoadData, func() error {
		if err := c.parseSourceCode(format, src, SourceRuntime); err != nil {
			return err
		}

		for _, sc := range more {
			if err := c.parseSourceCode(format, sc, SourceRuntime); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadStrings load one or multi string
func LoadStrings(format string, str string, more ...string) error {
	return dc.LoadStrings(format, str, more...)
}

// LoadStrings load data from source string content.
func (c *Config) LoadStrings(format string, str string, more ...string) (err error) {
	return c.writeAndHook(OnLoadData, func() error {
		if err := c.parseSourceCode(format, []byte(str), SourceRuntime); err != nil {
			return err
		}

		for _, s := range more {
			if err := c.parseSourceCode(format, []byte(s), SourceRuntime); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadFilesByFormat load one or multi config files by give format, will fire OnLoadData event
func LoadFilesByFormat(format string, configFiles ...string) error {
	return dc.LoadFilesByFormat(format, configFiles...)
}

// LoadFilesByFormat load one or multi files by give format, will fire OnLoadData event
func (c *Config) LoadFilesByFormat(format string, configFiles ...string) error {
	return c.writeAndHook(OnLoadData, func() error {
		return c.stageLoadedFiles(configFiles, repeatFormat(format, len(configFiles)), false)
	})
}

// LoadExistsByFormat load one or multi files by give format, will fire OnLoadData event
func LoadExistsByFormat(format string, configFiles ...string) error {
	return dc.LoadExistsByFormat(format, configFiles...)
}

// LoadExistsByFormat load one or multi files by give format, will fire OnLoadData event
func (c *Config) LoadExistsByFormat(format string, configFiles ...string) error {
	return c.writeAndHook(OnLoadData, func() error {
		return c.stageLoadedFiles(configFiles, repeatFormat(format, len(configFiles)), true)
	})
}

// repeatFormat builds a formats slice of length n filled with format, it pairs
// with Config.stageLoadedFiles.
func repeatFormat(format string, n int) []string {
	formats := make([]string, n)
	for i := range formats {
		formats[i] = format
	}
	return formats
}

// LoadOptions for load config from dir.
type LoadOptions struct {
	// DataKey use for load config from dir.
	// see https://github.com/gookit/config/issues/173
	DataKey string
}

// LoadOptFn type func
type LoadOptFn func(lo *LoadOptions)

func newLoadOptions(loFns []LoadOptFn) *LoadOptions {
	lo := &LoadOptions{}
	for _, fn := range loFns {
		fn(lo)
	}
	return lo
}

// LoadFromDir Load custom format files from the given directory, the file name will be used as the key.
//
// Example:
//
//	// file: /somedir/task.json
//	LoadFromDir("/somedir", "json")
//
//	// after load
//	Config.data = map[string]any{"task": file data}
func LoadFromDir(dirPath, format string, loFns ...LoadOptFn) error {
	return dc.LoadFromDir(dirPath, format, loFns...)
}

// LoadFromDir Load custom format files from the given directory, the file name will be used as the key.
//
// NOTE: will not be reloaded on call ReloadFiles(), if data loaded by the method.
//
// Example:
//
//	// file: /somedir/task.json , will use filename 'task' as key
//	Config.LoadFromDir("/somedir", "json")
//
//	// after load, the data will be:
//	Config.data = map[string]any{"task": {file data}}
func (c *Config) LoadFromDir(dirPath, format string, loFns ...LoadOptFn) (err error) {
	extName := "." + format
	extLen := len(extName)

	lo := newLoadOptions(loFns)
	dirData := make(map[string]any)
	dataList := make([]map[string]any, 0, 8)

	err = fsutil.FindInDir(dirPath, func(fPath string, ent fs.DirEntry) error {
		baseName := ent.Name()
		if strings.HasSuffix(baseName, extName) {
			data, err := c.parseSourceToMap(format, fsutil.MustReadFile(fPath))
			if err != nil {
				return err
			}

			// filename without ext.
			onlyName := baseName[:len(baseName)-extLen]
			if lo.DataKey != "" {
				dataList = append(dataList, data)
			} else {
				dirData[onlyName] = data
			}

			// TODO use file name as key, it cannot be reloaded. So, cannot append to loadedFiles
			// c.loadedFiles = append(c.loadedFiles, fPath)
		}
		return nil
	})

	if err != nil {
		return err
	}
	if lo.DataKey != "" {
		dirData[lo.DataKey] = dataList
	}

	if len(dirData) == 0 {
		return nil
	}

	return c.writeAndHook(OnLoadData, func() error {
		return c.loadDataMap(dirData, SourceRuntime)
	})
}

// ReloadFiles reload config data use loaded files
func ReloadFiles() error { return dc.ReloadFiles() }

// ReloadFiles reload config data use loaded files. use on watching loaded files change
//
// The values, their sources and the loaded files records are published as one
// consistent state: on any file read or parse error the previous successful
// state is kept, the OnReloadData event is not fired, and keys removed from the
// files leave no data and no provenance behind after a successful reload.
func (c *Config) ReloadFiles() (err error) {
	files := c.loadedFiles
	if len(files) == 0 {
		return
	}

	// tip: the same lock order as Config.writeAndHook(), but the OnReloadData event is
	// not fired when the reload is failed.
	c.hookMu.Lock()
	defer c.hookMu.Unlock()

	c.lock.Lock()
	bakData := c.data
	bakSources := c.sources

	stageData := make(map[string]any)
	stageSources := make(map[string]any)
	flags := c.sourceMergeFlags()

	for _, file := range files {
		data, _, skipped, rErr := c.readLoadFile(file, "", false)
		if rErr != nil {
			err = rErr
			break
		}
		if skipped {
			continue
		}
		if mErr := mergo.Merge(&stageData, data, c.opts.MergeOptions...); mErr != nil {
			err = errorx.WithStack(mErr)
			break
		}
		// the records are absolute paths, use the record itself as the tag
		mergeSourceRoot(stageSources, tagSourceRoot(data, file), flags)
	}
	if err != nil {
		// revert to the back up state on error
		c.data = bakData
		c.sources = bakSources
	} else {
		c.data = stageData
		c.sources = stageSources
	}
	c.ClearCaches()
	c.lock.Unlock()

	if err == nil {
		c.fireHook(OnReloadData)
	}
	return
}

// stageLoadedFiles reads, parses and merges the given files into staging copies
// of the current state, and publishes data, provenance and loaded files records
// together only when the whole batch succeeded. Any read/parse/merge error keeps
// the previous successful state untouched. the caller must hold the data lock.
//
//   - formats may be nil, the format is then detected by the file extension.
//   - ignoreMissing skips files that do not exist (LoadExists).
func (c *Config) stageLoadedFiles(files []string, formats []string, ignoreMissing bool) error {
	stageData := deepCopyDataMap(c.data)
	stageSources := deepCopySourceRoot(c.sources)
	flags := c.sourceMergeFlags()

	addedRecords := make([]string, 0, len(files))
	for i, file := range files {
		format := ""
		if formats != nil {
			format = formats[i]
		}

		data, tag, skipped, err := c.readLoadFile(file, format, ignoreMissing)
		if err != nil {
			return err
		}
		if skipped {
			continue
		}

		if err := mergo.Merge(&stageData, data, c.opts.MergeOptions...); err != nil {
			return errorx.WithStack(err)
		}
		mergeSourceRoot(stageSources, tagSourceRoot(data, tag), flags)
		addedRecords = append(addedRecords, tag)
	}

	// publish the consistent state
	c.data = stageData
	c.sources = stageSources
	c.loadedFiles = append(c.loadedFiles, addedRecords...)
	return nil
}

// readLoadFile opens and parses one config file. the caller must hold the data
// lock. It returns:
//   - skipped=true when the file does not exist and ignoreMissing is true;
//   - data and tag (the file absolute path) on success.
func (c *Config) readLoadFile(file string, format string, ignoreMissing bool) (data map[string]any, tag string, skipped bool, err error) {
	fd, err := os.Open(file)
	if err != nil {
		// skip not exist file
		if os.IsNotExist(err) && ignoreMissing {
			return nil, "", true, nil
		}
		return nil, "", false, err
	}
	//noinspection GoUnhandledErrorResult
	defer fd.Close()

	// read file content
	bts, err := io.ReadAll(fd)
	if err != nil {
		return nil, "", false, err
	}

	// get format for file ext
	if format == "" {
		format = strings.Trim(filepath.Ext(file), ".")
	}

	data, err = c.parseSourceToMap(format, bts)
	if err != nil {
		return nil, "", false, err
	}
	return data, absFilePath(file), false, nil
}

// absFilePath resolves the absolute source path of a loaded file.
func absFilePath(file string) string {
	if abs, err := filepath.Abs(file); err == nil {
		return abs
	}
	return file
}

// parse config source code to Config. the caller must hold the data lock.
func (c *Config) parseSourceCode(format string, blob []byte, tag string) (err error) {
	data, err := c.parseSourceToMap(format, blob)
	if err != nil {
		return err
	}

	return c.loadDataMap(data, tag)
}

// loadDataMap merge the data map to config data, tagging every node with tag.
// the caller must hold the data lock, and fire the OnLoadData event by itself.
func (c *Config) loadDataMap(data map[string]any, tag string) (err error) {
	if err = mergo.Merge(&c.data, data, c.opts.MergeOptions...); err != nil {
		return errorx.WithStack(err)
	}
	mergeSourceRoot(c.sources, tagSourceRoot(data, tag), c.sourceMergeFlags())
	return nil
}

// parse config source code to Config.
func (c *Config) parseSourceToMap(format string, blob []byte) (map[string]any, error) {
	format = c.resolveFormat(format)
	decode := c.decoders[format]
	if decode == nil {
		return nil, errors.New("not register decoder for the format: " + format)
	}

	if c.opts.Delimiter == 0 {
		c.opts.Delimiter = defaultDelimiter
	}

	// decode content to data
	data := make(map[string]any)

	if err := decode(blob, &data); err != nil {
		return nil, err
	}
	return data, nil
}

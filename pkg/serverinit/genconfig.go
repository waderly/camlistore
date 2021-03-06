/*
Copyright 2012 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package serverinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/jsonconfig"
	"camlistore.org/pkg/jsonsign"
	"camlistore.org/pkg/osutil"
	"camlistore.org/pkg/types/serverconfig"
)

// various parameters derived from the high-level user config
// and needed to set up the low-level config.
type configPrefixesParams struct {
	secretRing       string
	keyId            string
	haveIndex        bool
	haveSQLite       bool
	blobPath         string
	packBlobs        bool
	searchOwner      blob.Ref
	shareHandlerPath string
	flickr           string
	picasa           string
	memoryIndex      bool

	indexFileDir string // if sqlite or kvfile, its directory. else "".
}

var (
	tempDir = os.TempDir
	noMkdir bool // for tests to not call os.Mkdir
)

type tlsOpts struct {
	httpsCert string
	httpsKey  string
}

func addPublishedConfig(prefixes jsonconfig.Obj,
	published map[string]*serverconfig.Publish,
	sourceRoot string, tlsO *tlsOpts) ([]string, error) {
	var pubPrefixes []string
	for k, v := range published {
		if v.CamliRoot == "" {
			return nil, fmt.Errorf("Missing \"camliRoot\" key in configuration for %s.", k)
		}
		if v.GoTemplate == "" {
			return nil, fmt.Errorf("Missing \"goTemplate\" key in configuration for %s.", k)
		}
		ob := map[string]interface{}{}
		ob["handler"] = "app"

		appConfig := map[string]interface{}{
			"camliRoot":  v.CamliRoot,
			"cacheRoot":  v.CacheRoot,
			"goTemplate": v.GoTemplate,
		}
		if v.HTTPSCert != "" && v.HTTPSKey != "" {
			// user can specify these directly in the publish section
			appConfig["httpsCert"] = v.HTTPSCert
			appConfig["httpsKey"] = v.HTTPSKey
		} else {
			// default to Camlistore parameters, if any
			if tlsO != nil {
				appConfig["httpsCert"] = tlsO.httpsCert
				appConfig["httpsKey"] = tlsO.httpsKey
			}
		}

		handlerArgs := map[string]interface{}{
			"program":   v.Program,
			"appConfig": appConfig,
		}
		if v.BaseURL != "" {
			handlerArgs["baseURL"] = v.BaseURL
		}
		program := "publisher"
		if v.Program != "" {
			program = v.Program
		}
		handlerArgs["program"] = program

		ob["handlerArgs"] = handlerArgs
		prefixes[k] = ob
		pubPrefixes = append(pubPrefixes, k)
	}
	sort.Strings(pubPrefixes)
	return pubPrefixes, nil
}

func addUIConfig(params *configPrefixesParams,
	prefixes jsonconfig.Obj,
	uiPrefix string,
	sourceRoot string) {

	args := map[string]interface{}{
		"jsonSignRoot": "/sighelper/",
		"cache":        "/cache/",
	}
	if sourceRoot != "" {
		args["sourceRoot"] = sourceRoot
	}
	if params.blobPath != "" {
		args["scaledImage"] = map[string]interface{}{
			"type": "kv",
			"file": filepath.Join(params.blobPath, "thumbmeta.kv"),
		}
	}
	prefixes[uiPrefix] = map[string]interface{}{
		"handler":     "ui",
		"handlerArgs": args,
	}
}

func addMongoConfig(prefixes jsonconfig.Obj, dbname string, dbinfo string) {
	fields := strings.Split(dbinfo, "@")
	if len(fields) != 2 {
		exitFailure("Malformed mongo config string. Got \"%v\", want: \"user:password@host\"", dbinfo)
	}
	host := fields[1]
	fields = strings.Split(fields[0], ":")
	if len(fields) != 2 {
		exitFailure("Malformed mongo config string. Got \"%v\", want: \"user:password\"", fields[0])
	}
	ob := map[string]interface{}{}
	ob["enabled"] = true
	ob["handler"] = "storage-index"
	ob["handlerArgs"] = map[string]interface{}{
		"blobSource": "/bs/",
		"storage": map[string]interface{}{
			"type":     "mongo",
			"host":     host,
			"user":     fields[0],
			"password": fields[1],
			"database": dbname,
		},
	}
	prefixes["/index/"] = ob
}

func addSQLConfig(rdbms string, prefixes jsonconfig.Obj, dbname string, dbinfo string) {
	fields := strings.Split(dbinfo, "@")
	if len(fields) != 2 {
		exitFailure("Malformed " + rdbms + " config string. Want: \"user@host:password\"")
	}
	user := fields[0]
	fields = strings.Split(fields[1], ":")
	if len(fields) != 2 {
		exitFailure("Malformed " + rdbms + " config string. Want: \"user@host:password\"")
	}
	ob := map[string]interface{}{}
	ob["enabled"] = true
	ob["handler"] = "storage-index"
	ob["handlerArgs"] = map[string]interface{}{
		"blobSource": "/bs/",
		"storage": map[string]interface{}{
			"type":     rdbms,
			"host":     fields[0],
			"user":     user,
			"password": fields[1],
			"database": dbname,
		},
	}
	prefixes["/index/"] = ob
}

func addPostgresConfig(prefixes jsonconfig.Obj, dbname string, dbinfo string) {
	addSQLConfig("postgres", prefixes, dbname, dbinfo)
}

func addMySQLConfig(prefixes jsonconfig.Obj, dbname string, dbinfo string) {
	addSQLConfig("mysql", prefixes, dbname, dbinfo)
}

func addSQLiteConfig(prefixes jsonconfig.Obj, file string) {
	ob := map[string]interface{}{}
	ob["handler"] = "storage-index"
	ob["handlerArgs"] = map[string]interface{}{
		"blobSource": "/bs/",
		"storage": map[string]interface{}{
			"type": "sqlite",
			"file": file,
		},
	}
	prefixes["/index/"] = ob
}

func addKVConfig(prefixes jsonconfig.Obj, file string) {
	prefixes["/index/"] = map[string]interface{}{
		"handler": "storage-index",
		"handlerArgs": map[string]interface{}{
			"blobSource": "/bs/",
			"storage": map[string]interface{}{
				"type": "kv",
				"file": file,
			},
		},
	}
}

func addS3Config(params *configPrefixesParams, prefixes jsonconfig.Obj, s3 string) error {
	f := strings.SplitN(s3, ":", 4)
	if len(f) < 3 {
		return errors.New(`genconfig: expected "s3" field to be of form "access_key_id:secret_access_key:bucket"`)
	}
	accessKey, secret, bucket := f[0], f[1], f[2]
	var hostname string
	if len(f) == 4 {
		hostname = f[3]
	}
	isPrimary := false
	if _, ok := prefixes["/bs/"]; !ok {
		isPrimary = true
	}
	s3Prefix := ""
	if isPrimary {
		s3Prefix = "/bs/"
	} else {
		s3Prefix = "/sto-s3/"
	}
	args := map[string]interface{}{
		"aws_access_key":        accessKey,
		"aws_secret_access_key": secret,
		"bucket":                bucket,
	}
	if hostname != "" {
		args["hostname"] = hostname
	}
	prefixes[s3Prefix] = map[string]interface{}{
		"handler":     "storage-s3",
		"handlerArgs": args,
	}
	if isPrimary {
		// TODO(mpl): s3CacheBucket
		// See http://code.google.com/p/camlistore/issues/detail?id=85
		prefixes["/cache/"] = map[string]interface{}{
			"handler": "storage-filesystem",
			"handlerArgs": map[string]interface{}{
				"path": filepath.Join(tempDir(), "camli-cache"),
			},
		}
	} else {
		if params.blobPath == "" {
			panic("unexpected empty blobpath with sync-to-s3")
		}
		prefixes["/sync-to-s3/"] = map[string]interface{}{
			"handler": "sync",
			"handlerArgs": map[string]interface{}{
				"from": "/bs/",
				"to":   s3Prefix,
				"queue": map[string]interface{}{
					"type": "kv",
					"file": filepath.Join(params.blobPath, "sync-to-s3-queue.kv"),
				},
			},
		}
	}
	return nil
}

func addGoogleDriveConfig(params *configPrefixesParams, prefixes jsonconfig.Obj, highCfg string) error {
	f := strings.SplitN(highCfg, ":", 4)
	if len(f) != 4 {
		return errors.New(`genconfig: expected "googledrive" field to be of form "client_id:client_secret:refresh_token:parent_id"`)
	}
	clientId, secret, refreshToken, parentId := f[0], f[1], f[2], f[3]

	isPrimary := false
	if _, ok := prefixes["/bs/"]; !ok {
		isPrimary = true
	}

	prefix := ""
	if isPrimary {
		prefix = "/bs/"
	} else {
		prefix = "/sto-googledrive/"
	}
	prefixes[prefix] = map[string]interface{}{
		"handler": "storage-googledrive",
		"handlerArgs": map[string]interface{}{
			"parent_id": parentId,
			"auth": map[string]interface{}{
				"client_id":     clientId,
				"client_secret": secret,
				"refresh_token": refreshToken,
			},
		},
	}

	if isPrimary {
		prefixes["/cache/"] = map[string]interface{}{
			"handler": "storage-filesystem",
			"handlerArgs": map[string]interface{}{
				"path": filepath.Join(tempDir(), "camli-cache"),
			},
		}
	} else {
		prefixes["/sync-to-googledrive/"] = map[string]interface{}{
			"handler": "sync",
			"handlerArgs": map[string]interface{}{
				"from": "/bs/",
				"to":   prefix,
				"queue": map[string]interface{}{
					"type": "kv",
					"file": filepath.Join(params.blobPath,
						"sync-to-googledrive-queue.kv"),
				},
			},
		}
	}

	return nil
}

func addGoogleCloudStorageConfig(params *configPrefixesParams, prefixes jsonconfig.Obj, highCfg string) error {
	f := strings.SplitN(highCfg, ":", 4)
	if len(f) != 4 {
		return errors.New(`genconfig: expected "googlecloudstorage" field to be of form "client_id:client_secret:refresh_token:bucket"`)
	}
	clientId, secret, refreshToken, bucket := f[0], f[1], f[2], f[3]

	isPrimary := false
	if _, ok := prefixes["/bs/"]; !ok {
		isPrimary = true
	}

	gsPrefix := ""
	if isPrimary {
		gsPrefix = "/bs/"
	} else {
		gsPrefix = "/sto-googlecloudstorage/"
	}

	prefixes[gsPrefix] = map[string]interface{}{
		"handler": "storage-googlecloudstorage",
		"handlerArgs": map[string]interface{}{
			"bucket": bucket,
			"auth": map[string]interface{}{
				"client_id":     clientId,
				"client_secret": secret,
				"refresh_token": refreshToken,
				// If high-level config is for the common user then fullSyncOnStart = true
				// Then the default just works.
				//"fullSyncOnStart": true,
				//"blockingFullSyncOnStart": false
			},
		},
	}

	if isPrimary {
		// TODO: cacheBucket like s3CacheBucket?
		prefixes["/cache/"] = map[string]interface{}{
			"handler": "storage-filesystem",
			"handlerArgs": map[string]interface{}{
				"path": filepath.Join(tempDir(), "camli-cache"),
			},
		}
	} else {
		prefixes["/sync-to-googlecloudstorage/"] = map[string]interface{}{
			"handler": "sync",
			"handlerArgs": map[string]interface{}{
				"from": "/bs/",
				"to":   gsPrefix,
				"queue": map[string]interface{}{
					"type": "kv",
					"file": filepath.Join(params.blobPath,
						"sync-to-googlecloud-queue.kv"),
				},
			},
		}
	}
	return nil
}

func genLowLevelPrefixes(params *configPrefixesParams, ownerName string) (m jsonconfig.Obj) {
	m = make(jsonconfig.Obj)

	haveIndex := params.haveIndex
	root := "/bs/"
	pubKeyDest := root
	if haveIndex {
		root = "/bs-and-maybe-also-index/"
		pubKeyDest = "/bs-and-index/"
	}

	rootArgs := map[string]interface{}{
		"stealth":    false,
		"blobRoot":   root,
		"statusRoot": "/status/",
	}
	if ownerName != "" {
		rootArgs["ownerName"] = ownerName
	}
	m["/"] = map[string]interface{}{
		"handler":     "root",
		"handlerArgs": rootArgs,
	}
	if haveIndex {
		setMap(m, "/", "handlerArgs", "searchRoot", "/my-search/")
	}

	m["/setup/"] = map[string]interface{}{
		"handler": "setup",
	}

	m["/status/"] = map[string]interface{}{
		"handler": "status",
	}
	importerArgs := map[string]interface{}{}
	if haveIndex {
		m["/importer/"] = map[string]interface{}{
			"handler":     "importer",
			"handlerArgs": importerArgs,
		}
	}

	if params.shareHandlerPath != "" {
		m[params.shareHandlerPath] = map[string]interface{}{
			"handler": "share",
			"handlerArgs": map[string]interface{}{
				"blobRoot": "/bs/",
			},
		}
	}

	m["/sighelper/"] = map[string]interface{}{
		"handler": "jsonsign",
		"handlerArgs": map[string]interface{}{
			"secretRing":    params.secretRing,
			"keyId":         params.keyId,
			"publicKeyDest": pubKeyDest,
		},
	}

	storageType := "filesystem"
	if params.packBlobs {
		storageType = "diskpacked"
	}
	if params.blobPath != "" {
		m["/bs/"] = map[string]interface{}{
			"handler": "storage-" + storageType,
			"handlerArgs": map[string]interface{}{
				"path": params.blobPath,
			},
		}

		m["/cache/"] = map[string]interface{}{
			"handler": "storage-" + storageType,
			"handlerArgs": map[string]interface{}{
				"path": filepath.Join(params.blobPath, "/cache"),
			},
		}
	}

	if params.flickr != "" {
		importerArgs["flickr"] = map[string]interface{}{
			"clientSecret": params.flickr,
		}
	}
	if params.picasa != "" {
		importerArgs["picasa"] = map[string]interface{}{
			"clientSecret": params.picasa,
		}
	}

	if haveIndex {
		syncArgs := map[string]interface{}{
			"from": "/bs/",
			"to":   "/index/",
		}

		// TODO: currently when using s3, the index must be
		// sqlite or kvfile, since only through one of those
		// can we get a directory.
		if params.blobPath == "" && params.indexFileDir == "" {
			// We don't actually have a working sync handler, but we keep a stub registered
			// so it can be referred to from other places.
			// See http://camlistore.org/issue/201
			syncArgs["idle"] = true
		} else {
			dir := params.blobPath
			if dir == "" {
				dir = params.indexFileDir
			}
			typ := "kv"
			if params.haveSQLite {
				typ = "sqlite"
			}
			syncArgs["queue"] = map[string]interface{}{
				"type": typ,
				"file": filepath.Join(dir, "sync-to-index-queue."+typ),
			}
		}
		m["/sync/"] = map[string]interface{}{
			"handler":     "sync",
			"handlerArgs": syncArgs,
		}

		m["/bs-and-index/"] = map[string]interface{}{
			"handler": "storage-replica",
			"handlerArgs": map[string]interface{}{
				"backends": []interface{}{"/bs/", "/index/"},
			},
		}

		m["/bs-and-maybe-also-index/"] = map[string]interface{}{
			"handler": "storage-cond",
			"handlerArgs": map[string]interface{}{
				"write": map[string]interface{}{
					"if":   "isSchema",
					"then": "/bs-and-index/",
					"else": "/bs/",
				},
				"read": "/bs/",
			},
		}

		searchArgs := map[string]interface{}{
			"index": "/index/",
			"owner": params.searchOwner.String(),
		}
		if params.memoryIndex {
			searchArgs["slurpToMemory"] = true
		}
		m["/my-search/"] = map[string]interface{}{
			"handler":     "search",
			"handlerArgs": searchArgs,
		}
	}

	return
}

// genLowLevelConfig returns a low-level config from a high-level config.
func genLowLevelConfig(conf *serverconfig.Config) (lowLevelConf *Config, err error) {
	obj := jsonconfig.Obj{}
	if conf.HTTPS {
		if (conf.HTTPSCert != "") != (conf.HTTPSKey != "") {
			return nil, errors.New("Must set both httpsCert and httpsKey (or neither to generate a self-signed cert)")
		}
		if conf.HTTPSCert != "" {
			obj["httpsCert"] = conf.HTTPSCert
			obj["httpsKey"] = conf.HTTPSKey
		} else {
			obj["httpsCert"] = osutil.DefaultTLSCert()
			obj["httpsKey"] = osutil.DefaultTLSKey()
		}
	}

	if conf.BaseURL != "" {
		u, err := url.Parse(conf.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Error parsing baseURL %q as a URL: %v", conf.BaseURL, err)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("baseURL can't have a path, only a scheme, host, and optional port.")
		}
		u.Path = ""
		obj["baseURL"] = u.String()
	}
	if conf.Listen != "" {
		obj["listen"] = conf.Listen
	}
	obj["https"] = conf.HTTPS
	obj["auth"] = conf.Auth

	username := ""
	if conf.DBName == "" {
		username = osutil.Username()
		if username == "" {
			return nil, fmt.Errorf("USER (USERNAME on windows) env var not set; needed to define dbname")
		}
		conf.DBName = "camli" + username
	}

	var haveSQLite bool
	var indexFileDir string // filesystem directory of sqlite, kv, or similar
	numIndexers := numSet(conf.Mongo, conf.MySQL, conf.PostgreSQL, conf.SQLite, conf.KVFile)
	runIndex := conf.RunIndex.Get()

	switch {
	case runIndex && numIndexers == 0:
		return nil, fmt.Errorf("Unless runIndex is set to false, you must specify an index option (kvIndexFile, mongo, mysql, postgres, sqlite).")
	case runIndex && numIndexers != 1:
		return nil, fmt.Errorf("With runIndex set true, you can only pick exactly one indexer (mongo, mysql, postgres, sqlite).")
	case !runIndex && numIndexers != 0:
		return nil, fmt.Errorf("With runIndex disabled, you can't specify any of mongo, mysql, postgres, sqlite.")
	case conf.SQLite != "":
		haveSQLite = true
		indexFileDir = filepath.Dir(conf.SQLite)
	case conf.KVFile != "":
		indexFileDir = filepath.Dir(conf.KVFile)
	}

	entity, err := jsonsign.EntityFromSecring(conf.Identity, conf.IdentitySecretRing)
	if err != nil {
		return nil, err
	}
	armoredPublicKey, err := jsonsign.ArmoredPublicKey(entity)
	if err != nil {
		return nil, err
	}

	nolocaldisk := conf.BlobPath == ""
	if nolocaldisk {
		if conf.S3 == "" && conf.GoogleCloudStorage == "" {
			return nil, errors.New("You need at least one of blobPath (for localdisk) or s3 or googlecloudstorage configured for a blobserver.")
		}
		if conf.S3 != "" && conf.GoogleCloudStorage != "" {
			return nil, errors.New("Using S3 as a primary storage and Google Cloud Storage as a mirror is not supported for now.")
		}
	}

	if conf.ShareHandler && conf.ShareHandlerPath == "" {
		conf.ShareHandlerPath = "/share/"
	}

	prefixesParams := &configPrefixesParams{
		secretRing:       conf.IdentitySecretRing,
		keyId:            conf.Identity,
		haveIndex:        runIndex,
		haveSQLite:       haveSQLite,
		blobPath:         conf.BlobPath,
		packBlobs:        conf.PackBlobs,
		searchOwner:      blob.SHA1FromString(armoredPublicKey),
		shareHandlerPath: conf.ShareHandlerPath,
		flickr:           conf.Flickr,
		picasa:           conf.Picasa,
		memoryIndex:      conf.MemoryIndex.Get(),
		indexFileDir:     indexFileDir,
	}

	prefixes := genLowLevelPrefixes(prefixesParams, conf.OwnerName)
	var cacheDir string
	if nolocaldisk {
		// Whether camlistored is run from EC2 or not, we use
		// a temp dir as the cache when primary storage is S3.
		// TODO(mpl): s3CacheBucket
		// See http://code.google.com/p/camlistore/issues/detail?id=85
		cacheDir = filepath.Join(tempDir(), "camli-cache")
	} else {
		cacheDir = filepath.Join(conf.BlobPath, "cache")
	}
	if !noMkdir {
		if err := os.MkdirAll(cacheDir, 0700); err != nil {
			return nil, fmt.Errorf("Could not create blobs cache dir %s: %v", cacheDir, err)
		}
	}

	if len(conf.Publish) > 0 {
		if !runIndex {
			return nil, fmt.Errorf("publishing requires an index")
		}
		var tlsO *tlsOpts
		httpsCert, ok1 := obj["httpsCert"].(string)
		httpsKey, ok2 := obj["httpsKey"].(string)
		if ok1 && ok2 {
			tlsO = &tlsOpts{
				httpsCert: httpsCert,
				httpsKey:  httpsKey,
			}
		}
		_, err = addPublishedConfig(prefixes, conf.Publish, conf.SourceRoot, tlsO)
		if err != nil {
			return nil, fmt.Errorf("Could not generate config for published: %v", err)
		}
	}

	if runIndex {
		addUIConfig(prefixesParams, prefixes, "/ui/", conf.SourceRoot)
	}

	if conf.MySQL != "" {
		addMySQLConfig(prefixes, conf.DBName, conf.MySQL)
	}
	if conf.PostgreSQL != "" {
		addPostgresConfig(prefixes, conf.DBName, conf.PostgreSQL)
	}
	if conf.Mongo != "" {
		addMongoConfig(prefixes, conf.DBName, conf.Mongo)
	}
	if conf.SQLite != "" {
		addSQLiteConfig(prefixes, conf.SQLite)
	}
	if conf.KVFile != "" {
		addKVConfig(prefixes, conf.KVFile)
	}
	if conf.S3 != "" {
		if err := addS3Config(prefixesParams, prefixes, conf.S3); err != nil {
			return nil, err
		}
	}
	if conf.GoogleDrive != "" {
		if err := addGoogleDriveConfig(prefixesParams, prefixes, conf.GoogleDrive); err != nil {
			return nil, err
		}
	}
	if conf.GoogleCloudStorage != "" {
		if err := addGoogleCloudStorageConfig(prefixesParams, prefixes, conf.GoogleCloudStorage); err != nil {
			return nil, err
		}
	}

	obj["prefixes"] = (map[string]interface{})(prefixes)

	lowLevelConf = &Config{
		Obj: obj,
	}
	return lowLevelConf, nil
}

func numSet(vv ...interface{}) (num int) {
	for _, vi := range vv {
		switch v := vi.(type) {
		case string:
			if v != "" {
				num++
			}
		case bool:
			if v {
				num++
			}
		default:
			panic("unknown type")
		}
	}
	return
}

func setMap(m map[string]interface{}, v ...interface{}) {
	if len(v) < 2 {
		panic("too few args")
	}
	if len(v) == 2 {
		m[v[0].(string)] = v[1]
		return
	}
	setMap(m[v[0].(string)].(map[string]interface{}), v[1:]...)
}

// WriteDefaultConfigFile generates a new default high-level server configuration
// file at filePath. If useSQLite, the default indexer will use SQLite, otherwise
// kv. If filePath already exists, it is overwritten.
func WriteDefaultConfigFile(filePath string, useSQLite bool) error {
	conf := serverconfig.Config{
		Listen:      ":3179",
		HTTPS:       false,
		Auth:        "localhost",
		ReplicateTo: make([]interface{}, 0),
	}
	blobDir := osutil.CamliBlobRoot()
	if err := os.MkdirAll(blobDir, 0700); err != nil {
		return fmt.Errorf("Could not create default blobs directory: %v", err)
	}
	conf.BlobPath = blobDir
	if useSQLite {
		conf.SQLite = filepath.Join(osutil.CamliVarDir(), "camli-index.db")
	} else {
		conf.KVFile = filepath.Join(osutil.CamliVarDir(), "camli-index.kvdb")
	}

	var keyId string
	secRing := osutil.SecretRingFile()
	_, err := os.Stat(secRing)
	switch {
	case err == nil:
		keyId, err = jsonsign.KeyIdFromRing(secRing)
		if err != nil {
			return fmt.Errorf("Could not find any keyId in file %q: %v", secRing, err)
		}
		log.Printf("Re-using identity with keyId %q found in file %s", keyId, secRing)
	case os.IsNotExist(err):
		keyId, err = jsonsign.GenerateNewSecRing(secRing)
		if err != nil {
			return fmt.Errorf("Could not generate new secRing at file %q: %v", secRing, err)
		}
		log.Printf("Generated new identity with keyId %q in file %s", keyId, secRing)
	}
	if err != nil {
		return fmt.Errorf("Could not stat secret ring %q: %v", secRing, err)
	}
	conf.Identity = keyId
	conf.IdentitySecretRing = secRing

	confData, err := json.MarshalIndent(conf, "", "    ")
	if err != nil {
		return fmt.Errorf("Could not json encode config file : %v", err)
	}

	if err := ioutil.WriteFile(filePath, confData, 0600); err != nil {
		return fmt.Errorf("Could not create or write default server config: %v", err)
	}

	return nil
}

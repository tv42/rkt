// Copyright 2014 The rkt Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/rkt/pkg/lock"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/aci"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/jbenet/go-multihash"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/peterbourgon/diskv"
)

const (
	blobType int64 = iota
	imageManifestType

	defaultPathPerm os.FileMode = 0777
	defaultFilePerm os.FileMode = 0660

	// To ameliorate excessively long paths, keys for the (blob)store use
	// only the first half of a sha512 rather than the entire sum
	hashPrefix = "sha512-"
	lenHash    = sha512.Size       // raw byte size
	lenHashKey = (lenHash / 2) * 2 // half length, in hex characters
	lenKey     = len(hashPrefix) + lenHashKey
	minlenKey  = len(hashPrefix) + 2 // at least sha512-aa

	// how many backups to keep when migrating to new db version
	backupsNumber = 5
)

var diskvStores = [...]string{
	"blob",
	"imageManifest",
}

var (
	ErrKeyNotFound = errors.New("no keys found")
)

// StoreRemovalError defines an error removing a non transactional store (like
// a diskv store or the tree store).
// When this happen there's the possibility that the store is left in an
// unclean state (for example with some stale files).
type StoreRemovalError struct {
	errors []error
}

func (e *StoreRemovalError) Error() string {
	s := fmt.Sprintf("some aci disk entries cannot be removed: ")
	for _, err := range e.errors {
		s = s + fmt.Sprintf("[%v]", err)
	}
	return s
}

// Store encapsulates a content-addressable-storage for storing ACIs on disk.
type Store struct {
	dir       string
	stores    []*diskv.Diskv
	db        *DB
	treestore *TreeStore
	// storeLock is a lock on the whole store. It's used for store migration. If
	// a previous version of rkt is using the store and in the meantime a
	// new version is installed and executed it will try migrate the store
	// during NewStore. This means that the previous running rkt will fail
	// or behave badly after the migration as it's expecting another db format.
	// For this reason, before executing migration, an exclusive lock must
	// be taken on the whole store.
	storeLock        *lock.FileLock
	imageLockDir     string
	treeStoreLockDir string
}

func NewStore(baseDir string) (*Store, error) {
	storeDir := filepath.Join(baseDir, "cas")

	s := &Store{
		dir:    storeDir,
		stores: make([]*diskv.Diskv, len(diskvStores)),
	}

	s.imageLockDir = filepath.Join(storeDir, "imagelocks")
	err := os.MkdirAll(s.imageLockDir, defaultPathPerm)
	if err != nil {
		return nil, err
	}

	s.treeStoreLockDir = filepath.Join(storeDir, "treestorelocks")
	err = os.MkdirAll(s.treeStoreLockDir, defaultPathPerm)
	if err != nil {
		return nil, err
	}

	// Take a shared cas lock
	s.storeLock, err = lock.NewLock(storeDir, lock.Dir)
	if err != nil {
		return nil, err
	}

	for i, p := range diskvStores {
		s.stores[i] = diskv.New(diskv.Options{
			BasePath:  filepath.Join(storeDir, p),
			Transform: blockTransform,
		})
	}
	db, err := NewDB(filepath.Join(storeDir, "db"))
	if err != nil {
		return nil, err
	}
	s.db = db

	s.treestore = &TreeStore{path: filepath.Join(storeDir, "tree")}

	needsMigrate := false
	fn := func(tx *sql.Tx) error {
		var err error
		ok, err := dbIsPopulated(tx)
		if err != nil {
			return err
		}
		// populate the db
		if !ok {
			for _, stmt := range dbCreateStmts {
				_, err = tx.Exec(stmt)
				if err != nil {
					return err
				}
			}
			return nil
		}
		// if db is populated check its version
		version, err := getDBVersion(tx)
		if err != nil {
			return err
		}
		if version < dbVersion {
			needsMigrate = true
		}
		if version > dbVersion {
			return fmt.Errorf("Current store db version: %d greater than the current rkt expected version: %d", version, dbVersion)
		}
		return nil
	}
	if err = db.Do(fn); err != nil {
		return nil, err
	}

	// migration is done in another transaction as it must take an exclusive
	// store lock. If, in the meantime, another process has already done the
	// migration, between the previous db version check and the below
	// migration code, the migration will do nothing as it'll start
	// migration from the current version.
	if needsMigrate {
		// Take an exclusive store lock
		err := s.storeLock.ExclusiveLock()
		if err != nil {
			return nil, err
		}
		if err := s.backupDB(); err != nil {
			return nil, err
		}
		fn := func(tx *sql.Tx) error {
			return migrate(tx, dbVersion)
		}
		if err = db.Do(fn); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// backupDB backs up current database.
func (s Store) backupDB() error {
	backupsDir := filepath.Join(s.dir, "db-backups")
	return createBackup(s.db.dbdir, backupsDir, backupsNumber)
}

// TmpFile returns an *os.File local to the same filesystem as the Store, or
// any error encountered
func (s Store) TmpFile() (*os.File, error) {
	dir, err := s.TmpDir()
	if err != nil {
		return nil, err
	}
	return ioutil.TempFile(dir, "")
}

// TmpDir creates and returns dir local to the same filesystem as the Store,
// or any error encountered
func (s Store) TmpDir() (string, error) {
	dir := filepath.Join(s.dir, "tmp")
	if err := os.MkdirAll(dir, defaultPathPerm); err != nil {
		return "", err
	}
	return dir, nil
}

// ResolveKey resolves a partial key (of format `sha512-0c45e8c0ab2`) to a full
// key by considering the key a prefix and using the store for resolution.
// If the key is longer than the full key length, it is first truncated.
func (s Store) ResolveKey(key string) (string, error) {
	log.Printf("RESOLVEKEY %q", key)
	if !strings.HasPrefix(key, hashPrefix) {
		return "", fmt.Errorf("wrong key prefix")
	}
	if len(key) < minlenKey {
		return "", fmt.Errorf("key too short")
	}
	if len(key) > lenKey {
		key = key[:lenKey]
	}
	if len(key) == lenKey {
		return key, nil
	}

	aciInfos := []*ACIInfo{}
	err := s.db.Do(func(tx *sql.Tx) error {
		var err error
		aciInfos, err = GetACIInfosWithKeyPrefix(tx, key)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("error retrieving ACI Infos: %v", err)
	}

	keyCount := len(aciInfos)
	if keyCount == 0 {
		return "", ErrKeyNotFound
	}
	if keyCount != 1 {
		return "", fmt.Errorf("ambiguous key: %q", key)
	}
	return aciInfos[0].BlobKey, nil
}

func (s Store) ReadStream(key string) (io.ReadCloser, error) {
	log.Printf("READSTREAM %q", key)
	key, err := s.ResolveKey(key)
	if err != nil {
		return nil, fmt.Errorf("error resolving key: %v", err)
	}
	keyLock, err := lock.SharedKeyLock(s.imageLockDir, key)
	if err != nil {
		return nil, fmt.Errorf("error locking image: %v", err)
	}
	defer keyLock.Close()

	r, err := s.stores[blobType].ReadStream(key, false)
	if err != nil && os.IsNotExist(err) {
		log.Printf("BLOB NOPE %v", err)
		// try secondary source
		if r2, err2 := s.readStreamFromIPFS(key); err2 == nil {
			r, err = r2, err2
		}
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) readStreamFromIPFS(key string) (io.ReadCloser, error) {
	const prefix = "sha512-"
	if !strings.HasPrefix(key, prefix) {
		return nil, errors.New("only sha512 implemented in IPFS reader")
	}
	h, err := hex.DecodeString(key[len(prefix):])
	if err != nil {
		return nil, err
	}
	log.Printf("decoded %x", h)
	mhbuf, err := multihash.Encode(h[:32], multihash.SHA2_512)
	if err != nil {
		return nil, err
	}
	log.Printf("multihash %x", mhbuf)
	mh, err := multihash.Cast(mhbuf)
	if err != nil {
		return nil, err
	}
	// b58 will never require quoting
	log.Printf("b58 %v", mh.B58String())
	u := "http://localhost:5001/api/v0/block/get?arg=" + mh.B58String()
	log.Printf("GET %v", u)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	// IPFS likes to slam the socket shut, triggering
	// https://github.com/golang/go/issues/8946
	req.Close = true
	resp, err := http.DefaultClient.Do(req)
	log.Printf("GET GOT %v", err)
	if err != nil {
		return nil, err
	}
	log.Printf("GET STATUS %v", resp.Status)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http error: %v", resp.Status)
	}
	log.Printf("YAY")
	return resp.Body, nil
}

// WriteACI takes an ACI encapsulated in an io.Reader, decompresses it if
// necessary, and then stores it in the store under a key based on the image ID
// (i.e. the hash of the uncompressed ACI)
// latest defines if the aci has to be marked as the latest. For example an ACI
// discovered without asking for a specific version (latest pattern).
func (s Store) WriteACI(r io.ReadSeeker, latest bool) (string, error) {
	dr, err := aci.NewCompressedReader(r)
	if err != nil {
		return "", fmt.Errorf("error decompressing image: %v", err)
	}

	// Write the decompressed image (tar) to a temporary file on disk, and
	// tee so we can generate the hash
	h := sha512.New()
	tr := io.TeeReader(dr, h)
	fh, err := s.TmpFile()
	if err != nil {
		return "", fmt.Errorf("error creating image: %v", err)
	}
	if _, err := io.Copy(fh, tr); err != nil {
		return "", fmt.Errorf("error copying image: %v", err)
	}
	im, err := aci.ManifestFromImage(fh)
	if err != nil {
		return "", fmt.Errorf("error extracting image manifest: %v", err)
	}
	if err := fh.Close(); err != nil {
		return "", fmt.Errorf("error closing image: %v", err)
	}

	// Import the uncompressed image into the store at the real key
	key := s.HashToKey(h)
	keyLock, err := lock.ExclusiveKeyLock(s.imageLockDir, key)
	if err != nil {
		return "", fmt.Errorf("error locking image: %v", err)
	}
	defer keyLock.Close()

	if err = s.stores[blobType].Import(fh.Name(), key, true); err != nil {
		return "", fmt.Errorf("error importing image: %v", err)
	}

	// Save the imagemanifest using the same key used for the image
	imj, err := json.Marshal(im)
	if err != nil {
		return "", fmt.Errorf("error marshalling image manifest: %v", err)
	}
	if err = s.stores[imageManifestType].Write(key, imj); err != nil {
		return "", fmt.Errorf("error importing image manifest: %v", err)
	}

	// Save aciinfo
	if err = s.db.Do(func(tx *sql.Tx) error {
		aciinfo := &ACIInfo{
			BlobKey:    key,
			AppName:    im.Name.String(),
			ImportTime: time.Now(),
			Latest:     latest,
		}
		return WriteACIInfo(tx, aciinfo)
	}); err != nil {
		return "", fmt.Errorf("error writing ACI Info: %v", err)
	}

	// The treestore for this ACI is not written here as ACIs downloaded as
	// dependencies of another ACI will be exploded also if never directly used.
	// Users of treestore should call s.RenderTreeStore before using it.

	return key, nil
}

// RemoveACI removes the ACI with the given key. It firstly removes the aci
// infos inside the db, then it tries to remove the non transactional data.
// If some error occurs removing some non transactional data a
// StoreRemovalError is returned.
func (ds Store) RemoveACI(key string) error {
	imageKeyLock, err := lock.ExclusiveKeyLock(ds.imageLockDir, key)
	if err != nil {
		return fmt.Errorf("error locking image: %v", err)
	}
	defer imageKeyLock.Close()

	// Firstly remove aciinfo and remote from the db in an unique transaction.
	// remote needs to be removed or a GetRemote will return a blobKey not
	// referenced by any ACIInfo.
	err = ds.db.Do(func(tx *sql.Tx) error {
		if _, found, err := GetACIInfoWithBlobKey(tx, key); err != nil {
			return fmt.Errorf("error getting aciinfo: %v", err)
		} else if !found {
			return fmt.Errorf("cannot find image with key: %s", key)
		}

		if err := RemoveACIInfo(tx, key); err != nil {
			return err
		}
		if err := RemoveRemote(tx, key); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("cannot remove image with key: %s from db: %v", key, err)
	}

	// Then remove non transactional entries from the blob, imageManifest
	// and tree store.
	// TODO(sgotti). Now that the ACIInfo is removed the image doesn't
	// exists anymore, but errors removing non transactional entries can
	// leave stale data that will require a cas GC to be implemented.
	storeErrors := []error{}
	for _, s := range ds.stores {
		if err := s.Erase(key); err != nil {
			// If there's an error save it and continue with the other stores
			storeErrors = append(storeErrors, err)
		}
	}
	if len(storeErrors) > 0 {
		return &StoreRemovalError{errors: storeErrors}
	}
	return nil
}

// RenderTreeStore renders a treestore for the given image key if it's not
// already fully rendered.
// Users of treestore should call s.RenderTreeStore before using it to ensure
// that the treestore is completely rendered.
func (s Store) RenderTreeStore(key string, rebuild bool) error {
	// this lock references the treestore dir for the specified key. This
	// is different from a lock on an image key as internally
	// treestore.Write calls the acirenderer functions that use GetACI and
	// GetImageManifest which are taking the image(s) lock.
	treeStoreKeyLock, err := lock.ExclusiveKeyLock(s.treeStoreLockDir, key)
	if err != nil {
		return fmt.Errorf("error locking tree store: %v", err)
	}
	defer treeStoreKeyLock.Close()

	if !rebuild {
		rendered, err := s.treestore.IsRendered(key)
		if err != nil {
			return fmt.Errorf("cannot determine if tree is already rendered: %v", err)
		}
		if rendered {
			return nil
		}
	}
	// Firstly remove a possible partial treestore if existing.
	// This is needed as a previous ACI removal operation could have failed
	// cleaning the tree store leaving some stale files.
	if err := s.treestore.Remove(key); err != nil {
		return err
	}
	if err := s.treestore.Write(key, &s); err != nil {
		return fmt.Errorf("TREE STORE WRITE ERROR: %v", err)
	}
	return nil
}

// CheckTreeStore verifies the treestore consistency for the specified key.
func (s Store) CheckTreeStore(key string) error {
	treeStoreKeyLock, err := lock.SharedKeyLock(s.treeStoreLockDir, key)
	if err != nil {
		return fmt.Errorf("error locking tree store: %v", err)
	}
	defer treeStoreKeyLock.Close()

	return s.treestore.Check(key)
}

// GetTreeStorePath returns the absolute path of the treestore for the specified key.
// It doesn't ensure that the path exists and is fully rendered. This should
// be done calling IsRendered()
func (s Store) GetTreeStorePath(key string) string {
	return s.treestore.GetPath(key)
}

// GetTreeStoreRootFS returns the absolute path of the rootfs in the treestore
// for specified key.
// It doesn't ensure that the rootfs exists and is fully rendered. This should
// be done calling IsRendered()
func (s Store) GetTreeStoreRootFS(key string) string {
	return s.treestore.GetRootFS(key)
}

// RemoveTreeStore removes the rendered image in tree store with the given key.
func (ds Store) RemoveTreeStore(key string) error {
	treeStoreKeyLock, err := lock.ExclusiveKeyLock(ds.treeStoreLockDir, key)
	if err != nil {
		return fmt.Errorf("error locking tree store: %v", err)
	}
	defer treeStoreKeyLock.Close()

	if err := ds.treestore.Remove(key); err != nil {
		return fmt.Errorf("error removing the tree store: %v", err)
	}
	return nil
}

// GetRemote tries to retrieve a remote with the given ACIURL. found will be
// false if remote doesn't exist.
func (s Store) GetRemote(aciURL string) (*Remote, bool, error) {
	var remote *Remote
	found := false
	err := s.db.Do(func(tx *sql.Tx) error {
		var err error
		remote, found, err = GetRemote(tx, aciURL)
		return err
	})
	return remote, found, err
}

// WriteRemote adds or updates the provided Remote.
func (s Store) WriteRemote(remote *Remote) error {
	err := s.db.Do(func(tx *sql.Tx) error {
		return WriteRemote(tx, remote)
	})
	return err
}

// Get the ImageManifest with the specified key.
func (s Store) GetImageManifest(key string) (*schema.ImageManifest, error) {
	log.Printf("GETIMAGEMANIFEST %q", key)
	key, err := s.ResolveKey(key)
	if err != nil {
		return nil, fmt.Errorf("error resolving key: %v", err)
	}
	keyLock, err := lock.SharedKeyLock(s.imageLockDir, key)
	if err != nil {
		return nil, fmt.Errorf("error locking image: %v", err)
	}
	defer keyLock.Close()

	imj, err := s.stores[imageManifestType].Read(key)
	// TODO can't fetch manifests from a real CAS because they're
	// identified by the hash of the *image*, not of the manifest
	//
	// if err != nil && os.IsNotExist(err) {
	// 	log.Printf("BLOB NOPE %v", err)
	// 	// try secondary source
	// 	if imj2, err2 := s.readFromIPFS(key); err2 == nil {
	// 		imj, err = imj2, err2
	// 	}
	// }
	if err != nil {
		return nil, fmt.Errorf("error retrieving image manifest: %v", err)
	}
	log.Printf("MANIFEST %s", imj)
	var im *schema.ImageManifest
	if err = json.Unmarshal(imj, &im); err != nil {
		return nil, fmt.Errorf("error unmarshalling image manifest: %v", err)
	}
	return im, nil
}

func (s *Store) readFromIPFS(key string) ([]byte, error) {
	r, err := s.readStreamFromIPFS(key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return ioutil.ReadAll(r)
}

// GetACI retrieves the ACI that best matches the provided app name and labels.
// The returned value is the blob store key of the retrieved ACI.
// If there are multiple matching ACIs choose the latest one (defined as the
// last one imported in the store).
// If no version label is requested, ACIs marked as latest in the ACIInfo are
// preferred.
func (s Store) GetACI(name types.ACIdentifier, labels types.Labels) (string, error) {
	log.Printf("GetACI %v %v", name, labels)
	var curaciinfo *ACIInfo
	versionRequested := false
	if _, ok := labels.Get("version"); ok {
		versionRequested = true
	}

	var aciinfos []*ACIInfo
	err := s.db.Do(func(tx *sql.Tx) error {
		var err error
		aciinfos, _, err = GetACIInfosWithAppName(tx, name.String())
		return err
	})
	if err != nil {
		return "", err
	}

nextKey:
	for _, aciinfo := range aciinfos {
		im, err := s.GetImageManifest(aciinfo.BlobKey)
		if err != nil {
			return "", fmt.Errorf("error getting image manifest: %v", err)
		}

		// The image manifest must have all the requested labels
		for _, l := range labels {
			ok := false
			for _, rl := range im.Labels {
				if l.Name == rl.Name && l.Value == rl.Value {
					ok = true
					break
				}
			}
			if !ok {
				continue nextKey
			}
		}

		if curaciinfo != nil {
			// If no version is requested prefer the acis marked as latest
			if !versionRequested {
				if !curaciinfo.Latest && aciinfo.Latest {
					curaciinfo = aciinfo
					continue nextKey
				}
				if curaciinfo.Latest && !aciinfo.Latest {
					continue nextKey
				}
			}
			// If multiple matching image manifests are found, choose the latest imported in the cas.
			if aciinfo.ImportTime.After(curaciinfo.ImportTime) {
				curaciinfo = aciinfo
			}
		} else {
			curaciinfo = aciinfo
		}
	}

	if curaciinfo != nil {
		return curaciinfo.BlobKey, nil
	}
	return "", fmt.Errorf("cannot find aci satisfying name: %q and labels: %s in the local store", name, labelsToString(labels))
}

func (ds Store) GetAllACIInfos(sortfields []string, ascending bool) ([]*ACIInfo, error) {
	aciInfos := []*ACIInfo{}
	err := ds.db.Do(func(tx *sql.Tx) error {
		var err error
		aciInfos, err = GetAllACIInfos(tx, sortfields, ascending)
		return err
	})
	return aciInfos, err
}

func (s Store) Dump(hex bool) {
	for _, s := range s.stores {
		var keyCount int
		for key := range s.Keys(nil) {
			val, err := s.Read(key)
			if err != nil {
				panic(fmt.Sprintf("key %s had no value", key))
			}
			if len(val) > 128 {
				val = val[:128]
			}
			out := string(val)
			if hex {
				out = fmt.Sprintf("%x", val)
			}
			fmt.Printf("%s/%s: %s\n", s.BasePath, key, out)
			keyCount++
		}
		fmt.Printf("%d total keys\n", keyCount)
	}
}

// HashToKey takes a hash.Hash (which currently _MUST_ represent a full SHA512),
// calculates its sum, and returns a string which should be used as the key to
// store the data matching the hash.
func (s Store) HashToKey(h hash.Hash) string {
	return hashToKey(h)
}

func hashToKey(h hash.Hash) string {
	s := h.Sum(nil)
	return keyToString(s)
}

// keyToString takes a key and returns a shortened and prefixed hexadecimal string version
func keyToString(k []byte) string {
	if len(k) != lenHash {
		panic(fmt.Sprintf("bad hash passed to hashToKey: %x", k))
	}
	return fmt.Sprintf("%s%x", hashPrefix, k)[0:lenKey]
}

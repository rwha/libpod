package libpod

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containers/libpod/libpod/define"
	"github.com/containers/libpod/pkg/rootless"
	"github.com/containers/storage"
	bolt "github.com/etcd-io/bbolt"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	idRegistryName    = "id-registry"
	nameRegistryName  = "name-registry"
	nsRegistryName    = "ns-registry"
	ctrName           = "ctr"
	allCtrsName       = "all-ctrs"
	podName           = "pod"
	allPodsName       = "allPods"
	volName           = "vol"
	allVolsName       = "allVolumes"
	runtimeConfigName = "runtime-config"

	configName         = "config"
	stateName          = "state"
	dependenciesName   = "dependencies"
	volCtrDependencies = "vol-dependencies"
	netNSName          = "netns"
	containersName     = "containers"
	podIDName          = "pod-id"
	namespaceName      = "namespace"

	staticDirName   = "static-dir"
	tmpDirName      = "tmp-dir"
	runRootName     = "run-root"
	graphRootName   = "graph-root"
	graphDriverName = "graph-driver-name"
	osName          = "os"
	volPathName     = "volume-path"
)

var (
	idRegistryBkt    = []byte(idRegistryName)
	nameRegistryBkt  = []byte(nameRegistryName)
	nsRegistryBkt    = []byte(nsRegistryName)
	ctrBkt           = []byte(ctrName)
	allCtrsBkt       = []byte(allCtrsName)
	podBkt           = []byte(podName)
	allPodsBkt       = []byte(allPodsName)
	volBkt           = []byte(volName)
	allVolsBkt       = []byte(allVolsName)
	runtimeConfigBkt = []byte(runtimeConfigName)

	configKey          = []byte(configName)
	stateKey           = []byte(stateName)
	dependenciesBkt    = []byte(dependenciesName)
	volDependenciesBkt = []byte(volCtrDependencies)
	netNSKey           = []byte(netNSName)
	containersBkt      = []byte(containersName)
	podIDKey           = []byte(podIDName)
	namespaceKey       = []byte(namespaceName)

	staticDirKey   = []byte(staticDirName)
	tmpDirKey      = []byte(tmpDirName)
	runRootKey     = []byte(runRootName)
	graphRootKey   = []byte(graphRootName)
	graphDriverKey = []byte(graphDriverName)
	osKey          = []byte(osName)
	volPathKey     = []byte(volPathName)
)

// This represents a field in the runtime configuration that will be validated
// against the DB to ensure no configuration mismatches occur.
type dbConfigValidation struct {
	name         string // Only used for error messages
	runtimeValue string
	key          []byte
	defaultValue string
}

// Check if the configuration of the database is compatible with the
// configuration of the runtime opening it
// If there is no runtime configuration loaded, load our own
func checkRuntimeConfig(db *bolt.DB, rt *Runtime) error {
	storeOpts, err := storage.DefaultStoreOptions(rootless.IsRootless(), rootless.GetRootlessUID())
	if err != nil {
		return err
	}

	// We need to validate the following things
	checks := []dbConfigValidation{
		{
			"OS",
			runtime.GOOS,
			osKey,
			runtime.GOOS,
		},
		{
			"libpod root directory (staticdir)",
			rt.config.StaticDir,
			staticDirKey,
			"",
		},
		{
			"libpod temporary files directory (tmpdir)",
			rt.config.TmpDir,
			tmpDirKey,
			"",
		},
		{
			"storage temporary directory (runroot)",
			rt.config.StorageConfig.RunRoot,
			runRootKey,
			storeOpts.RunRoot,
		},
		{
			"storage graph root directory (graphroot)",
			rt.config.StorageConfig.GraphRoot,
			graphRootKey,
			storeOpts.GraphRoot,
		},
		{
			"storage graph driver",
			rt.config.StorageConfig.GraphDriverName,
			graphDriverKey,
			storeOpts.GraphDriverName,
		},
		{
			"volume path",
			rt.config.VolumePath,
			volPathKey,
			"",
		},
	}

	// These fields were missing and will have to be recreated.
	missingFields := []dbConfigValidation{}

	// Let's try and validate read-only first
	err = db.View(func(tx *bolt.Tx) error {
		configBkt, err := getRuntimeConfigBucket(tx)
		if err != nil {
			return err
		}

		for _, check := range checks {
			exists, err := readOnlyValidateConfig(configBkt, check)
			if err != nil {
				return err
			}
			if !exists {
				missingFields = append(missingFields, check)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if len(missingFields) == 0 {
		return nil
	}

	// Populate missing fields
	return db.Update(func(tx *bolt.Tx) error {
		configBkt, err := getRuntimeConfigBucket(tx)
		if err != nil {
			return err
		}

		for _, missing := range missingFields {
			dbValue := []byte(missing.runtimeValue)
			if missing.runtimeValue == "" && missing.defaultValue != "" {
				dbValue = []byte(missing.defaultValue)
			}

			if err := configBkt.Put(missing.key, dbValue); err != nil {
				return errors.Wrapf(err, "error updating %s in DB runtime config", missing.name)
			}
		}

		return nil
	})
}

// Attempt a read-only validation of a configuration entry in the DB against an
// element of the current runtime configuration.
// If the configuration key in question does not exist, (false, nil) will be
// returned.
// If the configuration key does exist, and matches the runtime configuration
// successfully, (true, nil) is returned.
// An error is only returned when validation fails.
// if the given runtimeValue or value retrieved from the database are empty,
// and defaultValue is not, defaultValue will be checked instead. This ensures
// that we will not fail on configuration changes in c/storage (where we may
// pass the empty string to use defaults).
func readOnlyValidateConfig(bucket *bolt.Bucket, toCheck dbConfigValidation) (bool, error) {
	keyBytes := bucket.Get(toCheck.key)
	if keyBytes == nil {
		// False return indicates missing key
		return false, nil
	}

	dbValue := string(keyBytes)

	if toCheck.runtimeValue != dbValue {
		// If the runtime value is the empty string and default is not,
		// check against default.
		if toCheck.runtimeValue == "" && toCheck.defaultValue != "" && dbValue == toCheck.defaultValue {
			return true, nil
		}

		// If the DB value is the empty string, check that the runtime
		// value is the default.
		if dbValue == "" && toCheck.defaultValue != "" && toCheck.runtimeValue == toCheck.defaultValue {
			return true, nil
		}

		return true, errors.Wrapf(define.ErrDBBadConfig, "database %s %q does not match our %s %q",
			toCheck.name, dbValue, toCheck.name, toCheck.runtimeValue)
	}

	return true, nil
}

// Open a connection to the database.
// Must be paired with a `defer closeDBCon()` on the returned database, to
// ensure the state is properly unlocked
func (s *BoltState) getDBCon() (*bolt.DB, error) {
	// We need an in-memory lock to avoid issues around POSIX file advisory
	// locks as described in the link below:
	// https://www.sqlite.org/src/artifact/c230a7a24?ln=994-1081
	s.dbLock.Lock()

	db, err := bolt.Open(s.dbPath, 0600, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "error opening database %s", s.dbPath)
	}

	return db, nil
}

// deferredCloseDBCon closes the bolt db but instead of returning an
// error it logs the error. it is meant to be used within the confines
// of a defer statement only
func (s *BoltState) deferredCloseDBCon(db *bolt.DB) {
	if err := s.closeDBCon(db); err != nil {
		logrus.Errorf("failed to close libpod db: %q", err)
	}
}

// Close a connection to the database.
// MUST be used in place of `db.Close()` to ensure proper unlocking of the
// state.
func (s *BoltState) closeDBCon(db *bolt.DB) error {
	err := db.Close()

	s.dbLock.Unlock()

	return err
}

func getIDBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(idRegistryBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "id registry bucket not found in DB")
	}
	return bkt, nil
}

func getNamesBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(nameRegistryBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "name registry bucket not found in DB")
	}
	return bkt, nil
}

func getNSBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(nsRegistryBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "namespace registry bucket not found in DB")
	}
	return bkt, nil
}

func getCtrBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(ctrBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "containers bucket not found in DB")
	}
	return bkt, nil
}

func getAllCtrsBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(allCtrsBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "all containers bucket not found in DB")
	}
	return bkt, nil
}

func getPodBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(podBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "pods bucket not found in DB")
	}
	return bkt, nil
}

func getAllPodsBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(allPodsBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "all pods bucket not found in DB")
	}
	return bkt, nil
}

func getVolBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(volBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "volumes bucket not found in DB")
	}
	return bkt, nil
}

func getAllVolsBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(allVolsBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "all volumes bucket not found in DB")
	}
	return bkt, nil
}

func getRuntimeConfigBucket(tx *bolt.Tx) (*bolt.Bucket, error) {
	bkt := tx.Bucket(runtimeConfigBkt)
	if bkt == nil {
		return nil, errors.Wrapf(define.ErrDBBadConfig, "runtime configuration bucket not found in DB")
	}
	return bkt, nil
}

func (s *BoltState) getContainerFromDB(id []byte, ctr *Container, ctrsBkt *bolt.Bucket) error {
	ctrBkt := ctrsBkt.Bucket(id)
	if ctrBkt == nil {
		return errors.Wrapf(define.ErrNoSuchCtr, "container %s not found in DB", string(id))
	}

	if s.namespaceBytes != nil {
		ctrNamespaceBytes := ctrBkt.Get(namespaceKey)
		if !bytes.Equal(s.namespaceBytes, ctrNamespaceBytes) {
			return errors.Wrapf(define.ErrNSMismatch, "cannot retrieve container %s as it is part of namespace %q and we are in namespace %q", string(id), string(ctrNamespaceBytes), s.namespace)
		}
	}

	configBytes := ctrBkt.Get(configKey)
	if configBytes == nil {
		return errors.Wrapf(define.ErrInternal, "container %s missing config key in DB", string(id))
	}

	if err := json.Unmarshal(configBytes, ctr.config); err != nil {
		return errors.Wrapf(err, "error unmarshalling container %s config", string(id))
	}

	// Get the lock
	lock, err := s.runtime.lockManager.RetrieveLock(ctr.config.LockID)
	if err != nil {
		return errors.Wrapf(err, "error retrieving lock for container %s", string(id))
	}
	ctr.lock = lock

	if ctr.config.OCIRuntime == "" {
		ctr.ociRuntime = s.runtime.defaultOCIRuntime
	} else {
		// Handle legacy containers which might use a literal path for
		// their OCI runtime name.
		runtimeName := ctr.config.OCIRuntime
		if strings.HasPrefix(runtimeName, "/") {
			runtimeName = filepath.Base(runtimeName)
		}

		ociRuntime, ok := s.runtime.ociRuntimes[runtimeName]
		if !ok {
			return errors.Wrapf(define.ErrInternal, "container %s was created with OCI runtime %s, but that runtime is not available in the current configuration", ctr.ID(), ctr.config.OCIRuntime)
		}
		ctr.ociRuntime = ociRuntime
	}

	ctr.runtime = s.runtime
	ctr.valid = true

	return nil
}

func (s *BoltState) getPodFromDB(id []byte, pod *Pod, podBkt *bolt.Bucket) error {
	podDB := podBkt.Bucket(id)
	if podDB == nil {
		return errors.Wrapf(define.ErrNoSuchPod, "pod with ID %s not found", string(id))
	}

	if s.namespaceBytes != nil {
		podNamespaceBytes := podDB.Get(namespaceKey)
		if !bytes.Equal(s.namespaceBytes, podNamespaceBytes) {
			return errors.Wrapf(define.ErrNSMismatch, "cannot retrieve pod %s as it is part of namespace %q and we are in namespace %q", string(id), string(podNamespaceBytes), s.namespace)
		}
	}

	podConfigBytes := podDB.Get(configKey)
	if podConfigBytes == nil {
		return errors.Wrapf(define.ErrInternal, "pod %s is missing configuration key in DB", string(id))
	}

	if err := json.Unmarshal(podConfigBytes, pod.config); err != nil {
		return errors.Wrapf(err, "error unmarshalling pod %s config from DB", string(id))
	}

	// Get the lock
	lock, err := s.runtime.lockManager.RetrieveLock(pod.config.LockID)
	if err != nil {
		return errors.Wrapf(err, "error retrieving lock for pod %s", string(id))
	}
	pod.lock = lock

	pod.runtime = s.runtime
	pod.valid = true

	return nil
}

func (s *BoltState) getVolumeFromDB(name []byte, volume *Volume, volBkt *bolt.Bucket) error {
	volDB := volBkt.Bucket(name)
	if volDB == nil {
		return errors.Wrapf(define.ErrNoSuchVolume, "volume with name %s not found", string(name))
	}

	volConfigBytes := volDB.Get(configKey)
	if volConfigBytes == nil {
		return errors.Wrapf(define.ErrInternal, "volume %s is missing configuration key in DB", string(name))
	}

	if err := json.Unmarshal(volConfigBytes, volume.config); err != nil {
		return errors.Wrapf(err, "error unmarshalling volume %s config from DB", string(name))
	}

	// Get the lock
	lock, err := s.runtime.lockManager.RetrieveLock(volume.config.LockID)
	if err != nil {
		return errors.Wrapf(err, "error retrieving lock for volume %q", string(name))
	}
	volume.lock = lock

	volume.runtime = s.runtime
	volume.valid = true

	return nil
}

// Add a container to the DB
// If pod is not nil, the container is added to the pod as well
func (s *BoltState) addContainer(ctr *Container, pod *Pod) error {
	if s.namespace != "" && s.namespace != ctr.config.Namespace {
		return errors.Wrapf(define.ErrNSMismatch, "cannot add container %s as it is in namespace %q and we are in namespace %q",
			ctr.ID(), s.namespace, ctr.config.Namespace)
	}

	// JSON container structs to insert into DB
	// TODO use a higher-performance struct encoding than JSON
	configJSON, err := json.Marshal(ctr.config)
	if err != nil {
		return errors.Wrapf(err, "error marshalling container %s config to JSON", ctr.ID())
	}
	stateJSON, err := json.Marshal(ctr.state)
	if err != nil {
		return errors.Wrapf(err, "error marshalling container %s state to JSON", ctr.ID())
	}
	netNSPath := getNetNSPath(ctr)
	dependsCtrs := ctr.Dependencies()

	ctrID := []byte(ctr.ID())
	ctrName := []byte(ctr.Name())

	var ctrNamespace []byte
	if ctr.config.Namespace != "" {
		ctrNamespace = []byte(ctr.config.Namespace)
	}

	db, err := s.getDBCon()
	if err != nil {
		return err
	}
	defer s.deferredCloseDBCon(db)

	err = db.Update(func(tx *bolt.Tx) error {
		idsBucket, err := getIDBucket(tx)
		if err != nil {
			return err
		}

		namesBucket, err := getNamesBucket(tx)
		if err != nil {
			return err
		}

		nsBucket, err := getNSBucket(tx)
		if err != nil {
			return err
		}

		ctrBucket, err := getCtrBucket(tx)
		if err != nil {
			return err
		}

		allCtrsBucket, err := getAllCtrsBucket(tx)
		if err != nil {
			return err
		}

		volBkt, err := getVolBucket(tx)
		if err != nil {
			return err
		}

		// If a pod was given, check if it exists
		var podDB *bolt.Bucket
		var podCtrs *bolt.Bucket
		if pod != nil {
			podBucket, err := getPodBucket(tx)
			if err != nil {
				return err
			}

			podID := []byte(pod.ID())

			podDB = podBucket.Bucket(podID)
			if podDB == nil {
				pod.valid = false
				return errors.Wrapf(define.ErrNoSuchPod, "pod %s does not exist in database", pod.ID())
			}
			podCtrs = podDB.Bucket(containersBkt)
			if podCtrs == nil {
				return errors.Wrapf(define.ErrInternal, "pod %s does not have a containers bucket", pod.ID())
			}

			podNS := podDB.Get(namespaceKey)
			if !bytes.Equal(podNS, ctrNamespace) {
				return errors.Wrapf(define.ErrNSMismatch, "container %s is in namespace %s and pod %s is in namespace %s",
					ctr.ID(), ctr.config.Namespace, pod.ID(), pod.config.Namespace)
			}
		}

		// Check if we already have a container with the given ID and name
		idExist := idsBucket.Get(ctrID)
		if idExist != nil {
			return errors.Wrapf(define.ErrCtrExists, "ID %s is in use", ctr.ID())
		}
		nameExist := namesBucket.Get(ctrName)
		if nameExist != nil {
			return errors.Wrapf(define.ErrCtrExists, "name %s is in use", ctr.Name())
		}

		// No overlapping containers
		// Add the new container to the DB
		if err := idsBucket.Put(ctrID, ctrName); err != nil {
			return errors.Wrapf(err, "error adding container %s ID to DB", ctr.ID())
		}
		if err := namesBucket.Put(ctrName, ctrID); err != nil {
			return errors.Wrapf(err, "error adding container %s name (%s) to DB", ctr.ID(), ctr.Name())
		}
		if ctrNamespace != nil {
			if err := nsBucket.Put(ctrID, ctrNamespace); err != nil {
				return errors.Wrapf(err, "error adding container %s namespace (%q) to DB", ctr.ID(), ctr.Namespace())
			}
		}
		if err := allCtrsBucket.Put(ctrID, ctrName); err != nil {
			return errors.Wrapf(err, "error adding container %s to all containers bucket in DB", ctr.ID())
		}

		newCtrBkt, err := ctrBucket.CreateBucket(ctrID)
		if err != nil {
			return errors.Wrapf(err, "error adding container %s bucket to DB", ctr.ID())
		}

		if err := newCtrBkt.Put(configKey, configJSON); err != nil {
			return errors.Wrapf(err, "error adding container %s config to DB", ctr.ID())
		}
		if err := newCtrBkt.Put(stateKey, stateJSON); err != nil {
			return errors.Wrapf(err, "error adding container %s state to DB", ctr.ID())
		}
		if ctrNamespace != nil {
			if err := newCtrBkt.Put(namespaceKey, ctrNamespace); err != nil {
				return errors.Wrapf(err, "error adding container %s namespace to DB", ctr.ID())
			}
		}
		if pod != nil {
			if err := newCtrBkt.Put(podIDKey, []byte(pod.ID())); err != nil {
				return errors.Wrapf(err, "error adding container %s pod to DB", ctr.ID())
			}
		}
		if netNSPath != "" {
			if err := newCtrBkt.Put(netNSKey, []byte(netNSPath)); err != nil {
				return errors.Wrapf(err, "error adding container %s netns path to DB", ctr.ID())
			}
		}
		if _, err := newCtrBkt.CreateBucket(dependenciesBkt); err != nil {
			return errors.Wrapf(err, "error creating dependencies bucket for container %s", ctr.ID())
		}

		// Add dependencies for the container
		for _, dependsCtr := range dependsCtrs {
			depCtrID := []byte(dependsCtr)

			depCtrBkt := ctrBucket.Bucket(depCtrID)
			if depCtrBkt == nil {
				return errors.Wrapf(define.ErrNoSuchCtr, "container %s depends on container %s, but it does not exist in the DB", ctr.ID(), dependsCtr)
			}

			depCtrPod := depCtrBkt.Get(podIDKey)
			if pod != nil {
				// If we're part of a pod, make sure the dependency is part of the same pod
				if depCtrPod == nil {
					return errors.Wrapf(define.ErrInvalidArg, "container %s depends on container %s which is not in pod %s", ctr.ID(), dependsCtr, pod.ID())
				}

				if string(depCtrPod) != pod.ID() {
					return errors.Wrapf(define.ErrInvalidArg, "container %s depends on container %s which is in a different pod (%s)", ctr.ID(), dependsCtr, string(depCtrPod))
				}
			} else {
				// If we're not part of a pod, we cannot depend on containers in a pod
				if depCtrPod != nil {
					return errors.Wrapf(define.ErrInvalidArg, "container %s depends on container %s which is in a pod - containers not in pods cannot depend on containers in pods", ctr.ID(), dependsCtr)
				}
			}

			depNamespace := depCtrBkt.Get(namespaceKey)
			if !bytes.Equal(ctrNamespace, depNamespace) {
				return errors.Wrapf(define.ErrNSMismatch, "container %s in namespace %q depends on container %s in namespace %q - namespaces must match", ctr.ID(), ctr.config.Namespace, dependsCtr, string(depNamespace))
			}

			depCtrDependsBkt := depCtrBkt.Bucket(dependenciesBkt)
			if depCtrDependsBkt == nil {
				return errors.Wrapf(define.ErrInternal, "container %s does not have a dependencies bucket", dependsCtr)
			}
			if err := depCtrDependsBkt.Put(ctrID, ctrName); err != nil {
				return errors.Wrapf(err, "error adding ctr %s as dependency of container %s", ctr.ID(), dependsCtr)
			}
		}

		// Add ctr to pod
		if pod != nil && podCtrs != nil {
			if err := podCtrs.Put(ctrID, ctrName); err != nil {
				return errors.Wrapf(err, "error adding container %s to pod %s", ctr.ID(), pod.ID())
			}
		}

		// Add container to named volume dependencies buckets
		for _, vol := range ctr.config.NamedVolumes {
			volDB := volBkt.Bucket([]byte(vol.Name))
			if volDB == nil {
				return errors.Wrapf(define.ErrNoSuchVolume, "no volume with name %s found in database when adding container %s", vol.Name, ctr.ID())
			}

			ctrDepsBkt := volDB.Bucket(volDependenciesBkt)
			if depExists := ctrDepsBkt.Get(ctrID); depExists == nil {
				if err := ctrDepsBkt.Put(ctrID, ctrID); err != nil {
					return errors.Wrapf(err, "error adding container %s to volume %s dependencies", ctr.ID(), vol.Name)
				}
			}
		}

		return nil
	})
	return err
}

// Remove a container from the DB
// If pod is not nil, the container is treated as belonging to a pod, and
// will be removed from the pod as well
func (s *BoltState) removeContainer(ctr *Container, pod *Pod, tx *bolt.Tx) error {
	ctrID := []byte(ctr.ID())
	ctrName := []byte(ctr.Name())

	idsBucket, err := getIDBucket(tx)
	if err != nil {
		return err
	}

	namesBucket, err := getNamesBucket(tx)
	if err != nil {
		return err
	}

	ctrBucket, err := getCtrBucket(tx)
	if err != nil {
		return err
	}

	nsBucket, err := getNSBucket(tx)
	if err != nil {
		return err
	}

	allCtrsBucket, err := getAllCtrsBucket(tx)
	if err != nil {
		return err
	}

	volBkt, err := getVolBucket(tx)
	if err != nil {
		return err
	}

	// Does the pod exist?
	var podDB *bolt.Bucket
	if pod != nil {
		podBucket, err := getPodBucket(tx)
		if err != nil {
			return err
		}

		podID := []byte(pod.ID())

		podDB = podBucket.Bucket(podID)
		if podDB == nil {
			pod.valid = false
			return errors.Wrapf(define.ErrNoSuchPod, "no pod with ID %s found in DB", pod.ID())
		}
	}

	// Does the container exist?
	ctrExists := ctrBucket.Bucket(ctrID)
	if ctrExists == nil {
		ctr.valid = false
		return errors.Wrapf(define.ErrNoSuchCtr, "no container with ID %s found in DB", ctr.ID())
	}

	// Compare namespace
	// We can't remove containers not in our namespace
	if s.namespace != "" {
		if s.namespace != ctr.config.Namespace {
			return errors.Wrapf(define.ErrNSMismatch, "container %s is in namespace %q, does not match our namespace %q", ctr.ID(), ctr.config.Namespace, s.namespace)
		}
		if pod != nil && s.namespace != pod.config.Namespace {
			return errors.Wrapf(define.ErrNSMismatch, "pod %s is in namespace %q, does not match out namespace %q", pod.ID(), pod.config.Namespace, s.namespace)
		}
	}

	if podDB != nil && pod != nil {
		// Check if the container is in the pod, remove it if it is
		podCtrs := podDB.Bucket(containersBkt)
		if podCtrs == nil {
			// Malformed pod
			logrus.Errorf("pod %s malformed in database, missing containers bucket!", pod.ID())
		} else {
			ctrInPod := podCtrs.Get(ctrID)
			if ctrInPod == nil {
				return errors.Wrapf(define.ErrNoSuchCtr, "container %s is not in pod %s", ctr.ID(), pod.ID())
			}
			if err := podCtrs.Delete(ctrID); err != nil {
				return errors.Wrapf(err, "error removing container %s from pod %s", ctr.ID(), pod.ID())
			}
		}
	}

	// Does the container have dependencies?
	ctrDepsBkt := ctrExists.Bucket(dependenciesBkt)
	if ctrDepsBkt == nil {
		return errors.Wrapf(define.ErrInternal, "container %s does not have a dependencies bucket", ctr.ID())
	}
	deps := []string{}
	err = ctrDepsBkt.ForEach(func(id, value []byte) error {
		deps = append(deps, string(id))

		return nil
	})
	if err != nil {
		return err
	}
	if len(deps) != 0 {
		return errors.Wrapf(define.ErrCtrExists, "container %s is a dependency of the following containers: %s", ctr.ID(), strings.Join(deps, ", "))
	}

	if err := ctrBucket.DeleteBucket(ctrID); err != nil {
		return errors.Wrapf(define.ErrInternal, "error deleting container %s from DB", ctr.ID())
	}

	if err := idsBucket.Delete(ctrID); err != nil {
		return errors.Wrapf(err, "error deleting container %s ID in DB", ctr.ID())
	}

	if err := namesBucket.Delete(ctrName); err != nil {
		return errors.Wrapf(err, "error deleting container %s name in DB", ctr.ID())
	}
	if err := nsBucket.Delete(ctrID); err != nil {
		return errors.Wrapf(err, "error deleting container %s namespace in DB", ctr.ID())
	}
	if err := allCtrsBucket.Delete(ctrID); err != nil {
		return errors.Wrapf(err, "error deleting container %s from all containers bucket in DB", ctr.ID())
	}

	depCtrs := ctr.Dependencies()

	// Remove us from other container's dependencies
	for _, depCtr := range depCtrs {
		depCtrID := []byte(depCtr)

		depCtrBkt := ctrBucket.Bucket(depCtrID)
		if depCtrBkt == nil {
			// The dependent container has been removed
			// This should not be possible, and means the
			// state is inconsistent, but don't error
			// The container with inconsistent state is the
			// one being removed
			continue
		}

		depCtrDependsBkt := depCtrBkt.Bucket(dependenciesBkt)
		if depCtrDependsBkt == nil {
			// This is more serious - another container in
			// the state is inconsistent
			// Log it, continue removing
			logrus.Errorf("Container %s is missing dependencies bucket in DB", ctr.ID())
			continue
		}

		if err := depCtrDependsBkt.Delete(ctrID); err != nil {
			return errors.Wrapf(err, "error removing container %s as a dependency of container %s", ctr.ID(), depCtr)
		}
	}

	// Remove container from named volume dependencies buckets
	for _, vol := range ctr.config.NamedVolumes {
		volDB := volBkt.Bucket([]byte(vol.Name))
		if volDB == nil {
			// Let's assume the volume was already deleted and
			// continue to remove the container
			continue
		}

		ctrDepsBkt := volDB.Bucket(volDependenciesBkt)
		if depExists := ctrDepsBkt.Get(ctrID); depExists == nil {
			if err := ctrDepsBkt.Delete(ctrID); err != nil {
				return errors.Wrapf(err, "error deleting container %s dependency on volume %s", ctr.ID(), vol.Name)
			}
		}
	}

	return nil
}

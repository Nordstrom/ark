/*
Copyright 2017 Heptio Inc.

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

package restore

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/pkg/api/v1"

	api "github.com/heptio/ark/pkg/apis/ark/v1"
	"github.com/heptio/ark/pkg/client"
	"github.com/heptio/ark/pkg/cloudprovider"
	"github.com/heptio/ark/pkg/discovery"
	arkv1client "github.com/heptio/ark/pkg/generated/clientset/typed/ark/v1"
	"github.com/heptio/ark/pkg/restore/restorers"
	"github.com/heptio/ark/pkg/util/collections"
	"github.com/heptio/ark/pkg/util/kube"
)

// Restorer knows how to restore a backup.
type Restorer interface {
	// Restore restores the backup data from backupReader, returning warnings and errors.
	Restore(restore *api.Restore, backup *api.Backup, backupReader io.Reader, logFile io.Writer) (api.RestoreResult, api.RestoreResult)
}

var _ Restorer = &kubernetesRestorer{}

type gvString string
type kindString string

// kubernetesRestorer implements Restorer for restoring into a Kubernetes cluster.
type kubernetesRestorer struct {
	discoveryHelper    discovery.Helper
	dynamicFactory     client.DynamicFactory
	restorers          map[schema.GroupResource]restorers.ResourceRestorer
	backupService      cloudprovider.BackupService
	backupClient       arkv1client.BackupsGetter
	namespaceClient    corev1.NamespaceInterface
	resourcePriorities []string
	fileSystem         FileSystem
	logger             *logrus.Logger
}

// prioritizeResources takes a list of pre-prioritized resources and a full list of resources to restore,
// and returns an ordered list of GroupResource-resolved resources in the order that they should be
// restored.
func prioritizeResources(helper discovery.Helper, priorities []string, includedResources *collections.IncludesExcludes, logger *logrus.Logger) ([]schema.GroupResource, error) {
	var ret []schema.GroupResource

	// set keeps track of resolved GroupResource names
	set := sets.NewString()

	// start by resolving priorities into GroupResources and adding them to ret
	for _, r := range priorities {
		gr, err := helper.ResolveGroupResource(r)
		if err != nil {
			return nil, err
		}

		if !includedResources.ShouldInclude(gr.String()) {
			logger.WithField("groupResource", gr).Info("Not including resource")
			continue
		}

		ret = append(ret, gr)
		set.Insert(gr.String())
	}

	// go through everything we got from discovery and add anything not in "set" to byName
	var byName []schema.GroupResource
	for _, resourceGroup := range helper.Resources() {
		// will be something like storage.k8s.io/v1
		groupVersion, err := schema.ParseGroupVersion(resourceGroup.GroupVersion)
		if err != nil {
			return nil, err
		}

		for _, resource := range resourceGroup.APIResources {
			gr := groupVersion.WithResource(resource.Name).GroupResource()

			if !includedResources.ShouldInclude(gr.String()) {
				logger.WithField("groupResource", gr).Info("Not including resource")
				continue
			}

			if !set.Has(gr.String()) {
				byName = append(byName, gr)
			}
		}
	}

	// sort byName by name
	sort.Slice(byName, func(i, j int) bool {
		return byName[i].String() < byName[j].String()
	})

	// combine prioritized with by-name
	ret = append(ret, byName...)

	return ret, nil
}

// NewKubernetesRestorer creates a new kubernetesRestorer.
func NewKubernetesRestorer(
	discoveryHelper discovery.Helper,
	dynamicFactory client.DynamicFactory,
	customRestorers map[string]restorers.ResourceRestorer,
	backupService cloudprovider.BackupService,
	resourcePriorities []string,
	backupClient arkv1client.BackupsGetter,
	namespaceClient corev1.NamespaceInterface,
	logger *logrus.Logger,
) (Restorer, error) {
	r := make(map[schema.GroupResource]restorers.ResourceRestorer)
	for gr, restorer := range customRestorers {
		resolved, err := discoveryHelper.ResolveGroupResource(gr)
		if err != nil {
			return nil, err
		}
		r[resolved] = restorer
	}

	return &kubernetesRestorer{
		discoveryHelper:    discoveryHelper,
		dynamicFactory:     dynamicFactory,
		restorers:          r,
		backupService:      backupService,
		backupClient:       backupClient,
		namespaceClient:    namespaceClient,
		resourcePriorities: resourcePriorities,
		fileSystem:         &osFileSystem{},
		logger:             logger,
	}, nil
}

// Restore executes a restore into the target Kubernetes cluster according to the restore spec
// and using data from the provided backup/backup reader. Returns a warnings and errors RestoreResult,
// respectively, summarizing info about the restore.
func (kr *kubernetesRestorer) Restore(restore *api.Restore, backup *api.Backup, backupReader io.Reader, logFile io.Writer) (api.RestoreResult, api.RestoreResult) {
	// metav1.LabelSelectorAsSelector converts a nil LabelSelector to a
	// Nothing Selector, i.e. a selector that matches nothing. We want
	// a selector that matches everything. This can be accomplished by
	// passing a non-nil empty LabelSelector.
	ls := restore.Spec.LabelSelector
	if ls == nil {
		ls = &metav1.LabelSelector{}
	}

	selector, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return api.RestoreResult{}, api.RestoreResult{Ark: []string{err.Error()}}
	}

	// get resource includes-excludes
	resourceIncludesExcludes := collections.GenerateIncludesExcludes(
		restore.Spec.IncludedResources,
		restore.Spec.ExcludedResources,
		func(item string) string {
			gr, err := kr.discoveryHelper.ResolveGroupResource(item)
			if err != nil {
				kr.logger.WithError(err).WithField("resource", item).Error("Unable to resolve resource")
				return ""
			}

			return gr.String()
		},
	)

	prioritizedResources, err := prioritizeResources(kr.discoveryHelper, kr.resourcePriorities, resourceIncludesExcludes, kr.logger)
	if err != nil {
		return api.RestoreResult{}, api.RestoreResult{Ark: []string{err.Error()}}
	}

	gzippedLog := gzip.NewWriter(logFile)
	defer gzippedLog.Close()

	ctx := &context{
		backup:               backup,
		backupReader:         backupReader,
		restore:              restore,
		prioritizedResources: prioritizedResources,
		selector:             selector,
		logger:               &logger{w: gzippedLog},
		dynamicFactory:       kr.dynamicFactory,
		fileSystem:           kr.fileSystem,
		namespaceClient:      kr.namespaceClient,
		restorers:            kr.restorers,
	}

	return ctx.execute()
}

type logger struct {
	w io.Writer
}

func (l *logger) log(msg string, args ...interface{}) {
	// TODO use a real logger that supports writing to files
	now := time.Now().Format(time.RFC3339)
	fmt.Fprintf(l.w, now+" "+msg+"\n", args...)
}

type context struct {
	backup               *api.Backup
	backupReader         io.Reader
	restore              *api.Restore
	prioritizedResources []schema.GroupResource
	selector             labels.Selector
	logger               *logger
	dynamicFactory       client.DynamicFactory
	fileSystem           FileSystem
	namespaceClient      corev1.NamespaceInterface
	restorers            map[schema.GroupResource]restorers.ResourceRestorer
}

func (ctx *context) log(msg string, args ...interface{}) {
	ctx.logger.log(msg, args...)
}

func (ctx *context) execute() (api.RestoreResult, api.RestoreResult) {
	ctx.log("Starting restore of backup %s", kube.NamespaceAndName(ctx.backup))

	dir, err := ctx.unzipAndExtractBackup(ctx.backupReader)
	if err != nil {
		ctx.log("error unzipping and extracting: %v", err)
		return api.RestoreResult{}, api.RestoreResult{Ark: []string{err.Error()}}
	}
	defer ctx.fileSystem.RemoveAll(dir)

	return ctx.restoreFromDir(dir)
}

// restoreFromDir executes a restore based on backup data contained within a local
// directory.
func (ctx *context) restoreFromDir(dir string) (api.RestoreResult, api.RestoreResult) {
	warnings, errors := api.RestoreResult{}, api.RestoreResult{}

	// cluster-scoped
	clusterPath := path.Join(dir, api.ClusterScopedDir)
	exists, err := ctx.fileSystem.DirExists(clusterPath)
	if err != nil {
		errors.Cluster = []string{err.Error()}
	}
	if exists {
		w, e := ctx.restoreNamespace("", clusterPath)
		merge(&warnings, &w)
		merge(&errors, &e)
	}

	// namespace-scoped
	namespacesPath := path.Join(dir, api.NamespaceScopedDir)
	exists, err = ctx.fileSystem.DirExists(namespacesPath)
	if err != nil {
		addArkError(&errors, err)
		return warnings, errors
	}
	if !exists {
		return warnings, errors
	}

	nses, err := ctx.fileSystem.ReadDir(namespacesPath)
	if err != nil {
		addArkError(&errors, err)
		return warnings, errors
	}

	namespaceFilter := collections.NewIncludesExcludes().Includes(ctx.restore.Spec.IncludedNamespaces...).Excludes(ctx.restore.Spec.ExcludedNamespaces...)
	for _, ns := range nses {
		if !ns.IsDir() {
			continue
		}
		nsPath := path.Join(namespacesPath, ns.Name())

		if !namespaceFilter.ShouldInclude(ns.Name()) {
			ctx.log("Skipping namespace %s", ns.Name())
			continue
		}

		w, e := ctx.restoreNamespace(ns.Name(), nsPath)
		merge(&warnings, &w)
		merge(&errors, &e)
	}

	return warnings, errors
}

// merge combines two RestoreResult objects into one
// by appending the corresponding lists to one another.
func merge(a, b *api.RestoreResult) {
	a.Cluster = append(a.Cluster, b.Cluster...)
	a.Ark = append(a.Ark, b.Ark...)
	for k, v := range b.Namespaces {
		if a.Namespaces == nil {
			a.Namespaces = make(map[string][]string)
		}
		a.Namespaces[k] = append(a.Namespaces[k], v...)
	}
}

// addArkError appends an error to the provided RestoreResult's Ark list.
func addArkError(r *api.RestoreResult, err error) {
	r.Ark = append(r.Ark, err.Error())
}

// addToResult appends an error to the provided RestoreResult, either within
// the cluster-scoped list (if ns == "") or within the provided namespace's
// entry.
func addToResult(r *api.RestoreResult, ns string, e error) {
	if ns == "" {
		r.Cluster = append(r.Cluster, e.Error())
	} else {
		if r.Namespaces == nil {
			r.Namespaces = make(map[string][]string)
		}
		r.Namespaces[ns] = append(r.Namespaces[ns], e.Error())
	}
}

// restoreNamespace restores the resources from a specified namespace directory in the backup,
// or from the cluster-scoped directory if no namespace is specified.
func (ctx *context) restoreNamespace(nsName, nsPath string) (api.RestoreResult, api.RestoreResult) {
	warnings, errors := api.RestoreResult{}, api.RestoreResult{}

	if nsName == "" {
		ctx.log("Restoring cluster-scoped resources")
	} else {
		ctx.log("Restoring namespace %s", nsName)
	}

	resourceDirs, err := ctx.fileSystem.ReadDir(nsPath)
	if err != nil {
		addToResult(&errors, nsName, err)
		return warnings, errors
	}

	resourceDirsMap := make(map[string]os.FileInfo)

	for _, rscDir := range resourceDirs {
		rscName := rscDir.Name()
		resourceDirsMap[rscName] = rscDir
	}

	if nsName != "" {
		// fetch mapped NS name
		if target, ok := ctx.restore.Spec.NamespaceMapping[nsName]; ok {
			nsName = target
		}

		// ensure namespace exists
		ns := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: nsName,
			},
		}

		if _, err := kube.EnsureNamespaceExists(ns, ctx.namespaceClient); err != nil {
			addArkError(&errors, err)
			return warnings, errors
		}
	}

	for _, resource := range ctx.prioritizedResources {
		rscDir := resourceDirsMap[resource.String()]
		if rscDir == nil {
			continue
		}

		resourcePath := path.Join(nsPath, rscDir.Name())

		w, e := ctx.restoreResourceForNamespace(nsName, resourcePath)
		merge(&warnings, &w)
		merge(&errors, &e)
	}

	return warnings, errors
}

// restoreResourceForNamespace restores the specified resource type for the specified
// namespace (or blank for cluster-scoped resources).
func (ctx *context) restoreResourceForNamespace(namespace string, resourcePath string) (api.RestoreResult, api.RestoreResult) {
	warnings, errors := api.RestoreResult{}, api.RestoreResult{}
	resource := path.Base(resourcePath)

	ctx.log("Restoring resource %v into namespace %v", resource, namespace)

	files, err := ctx.fileSystem.ReadDir(resourcePath)
	if err != nil {
		addToResult(&errors, namespace, fmt.Errorf("error reading %q resource directory: %v", resource, err))
		return warnings, errors
	}
	if len(files) == 0 {
		return warnings, errors
	}

	var (
		resourceClient client.Dynamic
		restorer       restorers.ResourceRestorer
		waiter         *resourceWaiter
		groupResource  = schema.ParseGroupResource(path.Base(resourcePath))
	)

	for _, file := range files {
		fullPath := filepath.Join(resourcePath, file.Name())
		obj, err := ctx.unmarshal(fullPath)
		if err != nil {
			addToResult(&errors, namespace, fmt.Errorf("error decoding %q: %v", fullPath, err))
			continue
		}

		if !ctx.selector.Matches(labels.Set(obj.GetLabels())) {
			continue
		}

		if restorer == nil {
			// initialize client & restorer for this Resource. we need
			// metadata from an object to do this.
			ctx.log("Getting client for %v", obj.GroupVersionKind())

			resource := metav1.APIResource{
				Namespaced: len(namespace) > 0,
				Name:       groupResource.Resource,
			}

			var err error
			resourceClient, err = ctx.dynamicFactory.ClientForGroupVersionKind(obj.GroupVersionKind(), resource, namespace)
			if err != nil {
				addArkError(&errors, fmt.Errorf("error getting resource client for namespace %q, resource %q: %v", namespace, &groupResource, err))
				return warnings, errors
			}

			restorer = ctx.restorers[groupResource]
			if restorer == nil {
				ctx.log("Using default restorer for %v", &groupResource)
				restorer = restorers.NewBasicRestorer(true)
			} else {
				ctx.log("Using custom restorer for %v", &groupResource)
			}

			if restorer.Wait() {
				itmWatch, err := resourceClient.Watch(metav1.ListOptions{})
				if err != nil {
					addArkError(&errors, fmt.Errorf("error watching for namespace %q, resource %q: %v", namespace, &groupResource, err))
					return warnings, errors
				}
				watchChan := itmWatch.ResultChan()
				defer itmWatch.Stop()

				waiter = newResourceWaiter(watchChan, restorer.Ready)
			}
		}

		if !restorer.Handles(obj, ctx.restore) {
			continue
		}

		if hasControllerOwner(obj.GetOwnerReferences()) {
			ctx.log("%s/%s has a controller owner - skipping", obj.GetNamespace(), obj.GetName())
			continue
		}

		preparedObj, warning, err := restorer.Prepare(obj, ctx.restore, ctx.backup)
		if warning != nil {
			addToResult(&warnings, namespace, fmt.Errorf("warning preparing %s: %v", fullPath, warning))
		}
		if err != nil {
			addToResult(&errors, namespace, fmt.Errorf("error preparing %s: %v", fullPath, err))
			continue
		}

		unstructuredObj, ok := preparedObj.(*unstructured.Unstructured)
		if !ok {
			addToResult(&errors, namespace, fmt.Errorf("%s: unexpected type %T", fullPath, preparedObj))
			continue
		}

		// necessary because we may have remapped the namespace
		unstructuredObj.SetNamespace(namespace)

		// add an ark-restore label to each resource for easy ID
		addLabel(unstructuredObj, api.RestoreLabelKey, ctx.restore.Name)

		ctx.log("Restoring %s: %v", obj.GroupVersionKind().Kind, unstructuredObj.GetName())
		_, err = resourceClient.Create(unstructuredObj)
		if apierrors.IsAlreadyExists(err) {
			addToResult(&warnings, namespace, err)
			continue
		}
		if err != nil {
			ctx.log("error restoring %s: %v", unstructuredObj.GetName(), err)
			addToResult(&errors, namespace, fmt.Errorf("error restoring %s: %v", fullPath, err))
			continue
		}

		if waiter != nil {
			waiter.RegisterItem(unstructuredObj.GetName())
		}
	}

	if waiter != nil {
		if err := waiter.Wait(); err != nil {
			addArkError(&errors, fmt.Errorf("error waiting for all %v resources to be created in namespace %s: %v", &groupResource, namespace, err))
		}
	}

	return warnings, errors
}

// addLabel applies the specified key/value to an object as a label.
func addLabel(obj *unstructured.Unstructured, key string, val string) {
	labels := obj.GetLabels()

	if labels == nil {
		labels = make(map[string]string)
	}

	labels[key] = val

	obj.SetLabels(labels)
}

// hasControllerOwner returns whether or not an object has a controller
// owner ref. Used to identify whether or not an object should be explicitly
// recreated during a restore.
func hasControllerOwner(refs []metav1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// unmarshal reads the specified file, unmarshals the JSON contained within it
// and returns an Unstructured object.
func (ctx *context) unmarshal(filePath string) (*unstructured.Unstructured, error) {
	var obj unstructured.Unstructured

	bytes, err := ctx.fileSystem.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(bytes, &obj)
	if err != nil {
		return nil, err
	}

	return &obj, nil
}

// unzipAndExtractBackup extracts a reader on a gzipped tarball to a local temp directory
func (ctx *context) unzipAndExtractBackup(src io.Reader) (string, error) {
	gzr, err := gzip.NewReader(src)
	if err != nil {
		ctx.log("error creating gzip reader: %v", err)
		return "", err
	}
	defer gzr.Close()

	return ctx.readBackup(tar.NewReader(gzr))
}

// readBackup extracts a tar reader to a local directory/file tree within a
// temp directory.
func (ctx *context) readBackup(tarRdr *tar.Reader) (string, error) {
	dir, err := ctx.fileSystem.TempDir("", "")
	if err != nil {
		ctx.log("error creating temp dir: %v", err)
		return "", err
	}

	for {
		header, err := tarRdr.Next()

		if err == io.EOF {
			break
		}
		if err != nil {
			ctx.log("error reading tar: %v", err)
			return "", err
		}

		target := path.Join(dir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			err := ctx.fileSystem.MkdirAll(target, header.FileInfo().Mode())
			if err != nil {
				ctx.log("mkdirall error: %v", err)
				return "", err
			}

		case tar.TypeReg:
			// make sure we have the directory created
			err := ctx.fileSystem.MkdirAll(path.Dir(target), header.FileInfo().Mode())
			if err != nil {
				ctx.log("mkdirall error: %v", err)
				return "", err
			}

			// create the file
			file, err := ctx.fileSystem.Create(target)
			if err != nil {
				return "", err
			}
			defer file.Close()

			if _, err := io.Copy(file, tarRdr); err != nil {
				ctx.log("error copying: %v", err)
				return "", err
			}
		}
	}

	return dir, nil
}

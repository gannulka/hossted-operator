package helm

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
)

type Helm struct {
	ReleaseName string
	Namespace   string
	Values      []string
	RepoName    string
	ChartName   string
	RepoUrl     string
}

func ListReleases(namespace string) ([]*release.Release, error) {
	settings := cli.New()

	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), log.Printf); err != nil {
		return nil, err
	}

	client := action.NewList(actionConfig)
	//client.Deployed = true

	return client.Run()
}

// Apply applies a Helm chart using the provided Helm struct configuration.
// It returns an error if any operation fails, otherwise, it returns nil.
func Apply(h Helm) error {

	settings := cli.New()

	// Initialize action configuration
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), h.Namespace, os.Getenv("HELM_DRIVER"), log.Printf); err != nil {
		return err
	}

	// Create a new Install action
	client := action.NewInstall(actionConfig)
	// Setting Namespace
	settings.SetNamespace(h.Namespace)
	settings.EnvVars()
	// Add repository
	repoAdd(h)

	//RepoUpdate()

	// Locate chart path
	cp, err := client.ChartPathOptions.LocateChart(fmt.Sprintf("%s/%s", h.RepoName, h.ChartName), settings)
	if err != nil {
		return err
	}

	// Load chart
	chartRequested, err := loader.Load(cp)
	if err != nil {
		return err
	}

	// Set action options
	client.ReleaseName = h.ChartName
	client.Namespace = h.Namespace
	client.CreateNamespace = true
	client.Wait = true
	client.Timeout = 120 * time.Second
	client.WaitForJobs = true
	//client.IncludeCRDs = true

	// Merge values
	values := values.Options{
		Values: h.Values,
	}

	vals, err := values.MergeValues(getter.All(settings))
	if err != nil {
		return err
	}
	// Run the Install action
	_, err = client.Run(chartRequested, vals)
	if err != nil {
		return err
	}
	return nil
}

// repoAdd adds a Helm repository.
// It takes a Helm struct as input containing the repository name and URL.
func repoAdd(h Helm) error {
	// Initialize CLI settings
	settings := cli.New()

	// Get the repository file path
	repoFile := settings.RepositoryConfig

	//Ensure the file directory exists as it is required for file locking
	err := os.MkdirAll(filepath.Dir(repoFile), os.ModePerm)
	if err != nil && !os.IsExist(err) {
		return err
	}

	// Acquire a file lock for process synchronization
	fileLock := flock.New(strings.Replace(repoFile, filepath.Ext(repoFile), ".lock", 1))
	lockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	locked, err := fileLock.TryLockContext(lockCtx, time.Second)

	if err == nil && locked {
		defer fileLock.Unlock()
	}

	if err != nil {
		return err
	}

	// Read the repository file
	b, err := ioutil.ReadFile(repoFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Unmarshal repository file content
	var f repo.File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return err
	}

	// Create a new repository entry
	c := repo.Entry{
		Name: h.RepoName,
		URL:  h.RepoUrl,
	}

	// Check if the repository is already added, update it
	if f.Has(h.RepoName) {
		r, err := repo.NewChartRepository(&c, getter.All(settings))
		if err != nil {
			return err
		}

		// Download the index file to update helm repo
		if _, err := r.DownloadIndexFile(); err != nil {
			err := errors.Wrapf(err, "looks like we are unable to update helm repo %q", h.RepoUrl)
			return err
		}
		return nil
	}
	// Create a new chart repository
	r, err := repo.NewChartRepository(&c, getter.All(settings))
	if err != nil {
		return err
	}

	// Download the index file
	if _, err := r.DownloadIndexFile(); err != nil {
		err := errors.Wrapf(err, "looks like %q is not a valid chart repository or cannot be reached", h.RepoUrl)
		return err
	}

	// Update repository file with the new entry
	f.Update(&c)

	// Write the updated repository file
	if err := f.WriteFile(repoFile, 0644); err != nil {
		return err
	}
	return nil
}

// ListRelease lists Helm releases based on the specified chart name and namespace.
// It returns an error if any operation fails, otherwise, it returns nil.
func ListRelease(chartName, namespace string) (bool, error) {
	settings := cli.New()

	// Initialize action configuration
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), log.Printf); err != nil {
		return false, err
	}

	// Create a new List action
	client := action.NewList(actionConfig)

	// Run the List action to get releases
	releases, err := client.Run()
	if err != nil {
		return false, err
	}

	// Iterate over the releases
	for _, release := range releases {
		// Check if the release's chart name matches the specified chart name
		if release.Chart.Name() == chartName {
			return true, nil
		}
	}

	// If no release with the specified chart name is found, return an error
	return false, nil
}

// DeleteRelease deletes a Helm release based on the specified chart name and namespace.
func DeleteRelease(chartName, namespace string) error {
	settings := cli.New()
	settings.SetNamespace(namespace)
	settings.EnvVars()
	// Initialize action configuration
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), log.Printf); err != nil {
		return err
	}

	// Create a new Uninstall action
	client := action.NewUninstall(actionConfig)
	// Run the Uninstall action to delete the release
	_, err := client.Run(chartName)
	if err != nil {
		return err
	}
	return nil
}

// RepoUpdate updates charts for all helm repos
func RepoUpdate() {
	var settings *cli.EnvSettings

	repoFile := settings.RepositoryConfig

	f, err := repo.LoadFile(repoFile)
	if os.IsNotExist(errors.Cause(err)) || len(f.Repositories) == 0 {
		log.Fatal(errors.New("no repositories found. You must add one before updating"))
	}
	var repos []*repo.ChartRepository
	for _, cfg := range f.Repositories {
		r, err := repo.NewChartRepository(cfg, getter.All(settings))
		if err != nil {
			log.Fatal(err)
		}
		repos = append(repos, r)
	}

	fmt.Printf("Hang tight while we grab the latest from your chart repositories...\n")
	var wg sync.WaitGroup
	for _, re := range repos {
		wg.Add(1)
		go func(re *repo.ChartRepository) {
			defer wg.Done()
			if _, err := re.DownloadIndexFile(); err != nil {
				fmt.Printf("...Unable to get an update from the %q chart repository (%s):\n\t%s\n", re.Config.Name, re.Config.URL, err)
			} else {
				fmt.Printf("...Successfully got an update from the %q chart repository\n", re.Config.Name)
			}
		}(re)
	}
	wg.Wait()
	fmt.Printf("Update Complete. ⎈ Happy Helming!⎈\n")
}

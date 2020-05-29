// Package clusterloader implements cluster loader.
// ref. https://github.com/kubernetes/perf-tests/tree/master/clusterloader2
package clusterloader

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	"github.com/aws/aws-k8s-tester/pkg/httputil"
	"go.uber.org/zap"
	"k8s.io/utils/exec"
)

// Config configures cluster loader.
type Config struct {
	Logger *zap.Logger
	Stopc  chan struct{}

	// KubeConfigPath is the kubeconfig path.
	// Optional. If empty, uses in-cluster client configuration.
	KubeConfigPath string

	// ClusterLoaderPath is the clusterloader executable binary path.
	// ref. https://github.com/kubernetes/perf-tests/tree/master/clusterloader2
	ClusterLoaderPath        string
	ClusterLoaderDownloadURL string
	// ClusterLoaderTestConfigPath is the clusterloader2 test configuration file.
	// Set via "--testconfig" flag.
	ClusterLoaderTestConfigPath string
	// ClusterLoaderReportDir is the clusterloader2 test report directory.
	// Set via "--report-dir" flag.
	ClusterLoaderReportDir string
	// ClusterLoaderLogsPath is the log file path to stream clusterloader binary runs.
	ClusterLoaderLogsPath string

	// Runs is the number of "clusterloader2" runs back-to-back.
	Runs    int
	Timeout time.Duration

	// Nodes is the number of nodes.
	// Set via "--nodes" flag.
	Nodes int

	//
	//
	// below are set via "--testoverrides" flag

	NodesPerNamespace int
	PodsPerNode       int

	BigGroupSize    int
	MediumGroupSize int
	SmallGroupSize  int

	SmallStatefulSetsPerNamespace  int
	MediumStatefulSetsPerNamespace int

	CL2EnablePVS              bool
	PrometheusScrapeKubeProxy bool
	EnableSystemPodMetrics    bool
}

// Loader defines cluster loader operations.
type Loader interface {
	Start() error
	Stop()
	GetResults()
}

type loader struct {
	cfg            Config
	donec          chan struct{}
	donecCloseOnce *sync.Once

	rootCtx           context.Context
	rootCancel        context.CancelFunc
	testOverridesPath string
}

func New(cfg Config) Loader {
	return &loader{
		cfg:               cfg,
		donec:             make(chan struct{}),
		donecCloseOnce:    new(sync.Once),
		testOverridesPath: "",
	}
}

func (ld *loader) Start() (err error) {
	ld.cfg.Logger.Info("starting cluster loader")

	if !fileutil.Exist(ld.cfg.ClusterLoaderTestConfigPath) {
		ld.cfg.Logger.Warn("clusterloader test config file does not exist", zap.String("path", ld.cfg.ClusterLoaderTestConfigPath))
		return fmt.Errorf("%q not found", ld.cfg.ClusterLoaderTestConfigPath)
	}

	if err = os.MkdirAll(ld.cfg.ClusterLoaderReportDir, 0700); err != nil {
		return err
	}
	if err = fileutil.IsDirWriteable(ld.cfg.ClusterLoaderReportDir); err != nil {
		return err
	}

	if err = ld.downloadClusterLoader(); err != nil {
		return err
	}
	if err = ld.writeTestOverrides(); err != nil {
		return err
	}

	args := []string{
		ld.cfg.ClusterLoaderPath,
		"--alsologtostderr",
		"--testconfig=" + ld.cfg.ClusterLoaderTestConfigPath,
		"--testoverrides=" + ld.testOverridesPath,
		"--report-dir=" + ld.cfg.ClusterLoaderReportDir,
		"--nodes=" + fmt.Sprintf("%d", ld.cfg.Nodes),
	}
	if ld.cfg.KubeConfigPath != "" {
		args = append(args, "--kubeconfig="+ld.cfg.KubeConfigPath)
	}
	cmd := strings.Join(args, " ")

	donec := make(chan struct{})
	ld.rootCtx, ld.rootCancel = context.WithTimeout(context.Background(), ld.cfg.Timeout)
	go func() {
		defer func() {
			close(donec)
		}()
		for i := 0; i < ld.cfg.Runs; i++ {
			select {
			case <-ld.rootCtx.Done():
				return
			default:
			}
			if err = ld.run(i, args, cmd); err != nil {
				return err
			}
		}
	}()
	select {
	case <-ld.cfg.Stopc:
		ld.cfg.Logger.Info("stopping cluster loader")
	case <-ld.rootCtx.Done():
		ld.cfg.Logger.Info("timed out cluster loader")
	case <-donec:
		ld.cfg.Logger.Info("completed cluster loader")
	}
	ld.rootCancel()

	return err
}

func (ld *loader) Stop() {
	ld.cfg.Logger.Info("stopping and waiting for cluster loader")
	ld.donecCloseOnce.Do(func() {
		close(ld.donec)
	})
	ld.cfg.Logger.Info("stopped and waited for cluster loader")
}

func (ld *loader) GetResults() {

}

func (ld *loader) downloadClusterLoader() (err error) {
	ld.cfg.Logger.Info("mkdir", zap.String("clusterloader-path-dir", filepath.Dir(ld.cfg.ClusterLoaderPath)))
	if err = os.MkdirAll(filepath.Dir(ld.cfg.ClusterLoaderPath), 0700); err != nil {
		return fmt.Errorf("could not create %q (%v)", filepath.Dir(ld.cfg.ClusterLoaderPath), err)
	}
	if !fileutil.Exist(ld.cfg.ClusterLoaderPath) {
		ld.cfg.ClusterLoaderPath, _ = filepath.Abs(ld.cfg.ClusterLoaderPath)
		ld.cfg.Logger.Info("downloading clusterloader", zap.String("clusterloader-path", ld.cfg.ClusterLoaderPath))
		if err = httputil.Download(ld.cfg.Logger, os.Stderr, ld.cfg.ClusterLoaderDownloadURL, ld.cfg.ClusterLoaderPath); err != nil {
			return err
		}
	} else {
		ld.cfg.Logger.Info("skipping clusterloader download; already exist", zap.String("clusterloader-path", ld.cfg.ClusterLoaderPath))
	}
	if err = fileutil.EnsureExecutable(ld.cfg.ClusterLoaderPath); err != nil {
		// file may be already executable while the process does not own the file/directory
		// ref. https://github.com/aws/aws-k8s-tester/issues/66
		ld.cfg.Logger.Warn("failed to ensure executable", zap.Error(err))
		err = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	output, herr := exec.New().CommandContext(
		ctx,
		ld.cfg.ClusterLoaderPath,
		"--help",
	).CombinedOutput()
	cancel()
	out := strings.TrimSpace(string(output))
	fmt.Printf("'%s --help' output:\n\n%s\n(error: %v)\n\n", ld.cfg.ClusterLoaderPath, out, herr)

	return err
}

func (ld *loader) writeTestOverrides() (err error) {
	buf := bytes.NewBuffer(nil)
	tpl := template.Must(template.New("TemplateTestOverrides").Parse(TemplateTestOverrides))
	if err := tpl.Execute(buf, ld.cfg); err != nil {
		return err
	}

	fmt.Printf("test overrides configuration:\n\n%s\n\n", buf.String())

	ld.testOverridesPath, err = fileutil.WriteTempFile(buf.Bytes())
	if err != nil {
		ld.cfg.Logger.Warn("failed to write", zap.Error(err))
		return err
	}

	ld.cfg.Logger.Info("wrote test overrides file", zap.String("path", ld.testOverridesPath))
	return nil
}

const TemplateTestOverrides = `NODES_PER_NAMESPACE: {{ .NodesPerNamespace }}
PODS_PER_NODE: {{ .PodsPerNode }}
BIG_GROUP_SIZE: {{ .BigGroupSize }}
MEDIUM_GROUP_SIZE: {{ .MediumGroupSize }}
SMALL_GROUP_SIZE: {{ .SmallGroupSize }}
SMALL_STATEFUL_SETS_PER_NAMESPACE: {{ .SmallStatefulSetsPerNamespace }}
MEDIUM_STATEFUL_SETS_PER_NAMESPACE: {{ .MediumStatefulSetsPerNamespace }}
CL2_ENABLE_PVS: {{ .CL2EnablePVS }}
PROMETHEUS_SCRAPE_KUBE_PROXY: {{ .PrometheusScrapeKubeProxy }}
ENABLE_SYSTEM_POD_METRICS: {{ .EnableSystemPodMetrics }}
`

func (ld *loader) run(idx int, args []string) (err error) {
	ld.cfg.Logger.Info("running cluster loader", zap.Int("index", idx), zap.String("command", strings.Join(args, " ")))
	ctx, cancel := context.WithTimeout(ld.rootCtx, 20*time.Minute)
	cmd := exec.New().CommandContext(ctx, args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	cancel()
	if err != nil {
		ld.cfg.Logger.Warn("failed to run cluster loader", zap.Error(err))
	}
	return err
}

package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Labels on every Pyre-managed resource, so listing and cleanup are exact.
const (
	LabelNode    = "pyre-node"
	LabelManaged = "pyre-managed"
	LabelRole    = "pyre-role"
	LabelOS      = "pyre-os"
	// NetworkTag selects the instances the deny-ingress firewall rule covers.
	NetworkTag   = "pyre-node"
	FirewallRule = "pyre-deny-ingress"
	// InstancePrefix: the VM of node N is pyre-N.
	InstancePrefix = "pyre-"
)

// Metadata keys. The two secrets are removed by the CLI once the node has
// connected (or the wait gave up); both are single-use and short-lived.
const (
	MetaStartupScript = "startup-script"
	MetaConfig        = "pyre-config"
	MetaEnrollSecret  = "pyre-enroll-secret"
	MetaTSAuthKey     = "pyre-ts-authkey"
	MetaNodeID        = "pyre-node-id"
	MetaIdleMinutes   = "pyre-idle-minutes"
)

// SecretMetadataKeys are never left on an instance after create/start.
var SecretMetadataKeys = []string{MetaEnrollSecret, MetaTSAuthKey}

// Runner runs the gcloud CLI. Arguments never carry a secret: secrets go
// through --metadata-from-file with 0600 files in a private temp dir.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRunner runs the real gcloud binary with the caller's environment
// (its active account and configuration). No credentials are stored.
type ExecRunner struct {
	Bin string // default "gcloud"
}

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "gcloud"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), &GcloudError{Args: redactArgs(args), Msg: msg}
	}
	return out.Bytes(), nil
}

// GcloudError is a failed gcloud invocation.
type GcloudError struct {
	Args []string
	Msg  string
}

func (e *GcloudError) Error() string {
	return "gcloud " + strings.Join(firstN(e.Args, 4), " ") + ": " + e.Msg
}

// NotFound reports a gcloud "not found" failure.
func NotFound(err error) bool {
	var ge *GcloudError
	if !errors.As(err, &ge) {
		return false
	}
	m := strings.ToLower(ge.Msg)
	return strings.Contains(m, "was not found") || strings.Contains(m, "notfound") || strings.Contains(m, "not found")
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// redactArgs is defensive: no secret is ever passed, but metadata values
// are dropped from error messages anyway.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "--metadata=") {
			a = "--metadata=…"
		}
		out[i] = a
	}
	return out
}

// GCP wraps gcloud for one project.
type GCP struct {
	Run     Runner
	Project string
}

func (g *GCP) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{}, args...)
	if g.Project != "" {
		full = append(full, "--project="+g.Project)
	}
	full = append(full, "--quiet")
	return g.Run.Run(ctx, full...)
}

// ConfigValue reads `gcloud config get-value KEY` ("" when unset).
func ConfigValue(ctx context.Context, r Runner, key string) string {
	out, err := r.Run(ctx, "config", "get-value", key, "--quiet")
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(out))
	if v == "(unset)" {
		return ""
	}
	return v
}

// Instance is the subset of a GCE instance Pyre uses.
type Instance struct {
	Name        string            `json:"name"`
	Node        string            `json:"node"` // pyre-node label
	Zone        string            `json:"zone"`
	MachineType string            `json:"machine_type"`
	Status      string            `json:"status"` // PROVISIONING STAGING RUNNING STOPPING SUSPENDED TERMINATED
	Spot        bool              `json:"spot,omitempty"`
	GPU         string            `json:"gpu,omitempty"` // "nvidia-l4:1"
	DiskGB      int               `json:"disk_gb,omitempty"`
	DiskType    string            `json:"disk_type,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	NodeID      string            `json:"node_id,omitempty"`
	IdleMinutes string            `json:"idle_minutes,omitempty"`
	// SecretsPending: a secret metadata key is still on the instance.
	SecretsPending bool `json:"secrets_pending,omitempty"`
}

type gceInstance struct {
	Name               string            `json:"name"`
	Zone               string            `json:"zone"`
	MachineType        string            `json:"machineType"`
	Status             string            `json:"status"`
	Labels             map[string]string `json:"labels"`
	CreationTimestamp  string            `json:"creationTimestamp"`
	LastStartTimestamp string            `json:"lastStartTimestamp"`
	Scheduling         struct {
		ProvisioningModel string `json:"provisioningModel"`
		Preemptible       bool   `json:"preemptible"`
	} `json:"scheduling"`
	GuestAccelerators []struct {
		AcceleratorType  string `json:"acceleratorType"`
		AcceleratorCount int    `json:"acceleratorCount"`
	} `json:"guestAccelerators"`
	Disks []struct {
		Boot       bool            `json:"boot"`
		DiskSizeGb json.RawMessage `json:"diskSizeGb"` // int64 as a JSON string
		Source     string          `json:"source"`
	} `json:"disks"`
	Metadata struct {
		Items []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"items"`
	} `json:"metadata"`
}

func (gi gceInstance) instance() Instance {
	in := Instance{Name: gi.Name, Zone: path.Base(gi.Zone), MachineType: path.Base(gi.MachineType), Status: gi.Status,
		Labels: gi.Labels, Node: gi.Labels[LabelNode],
		Spot: gi.Scheduling.ProvisioningModel == "SPOT" || gi.Scheduling.Preemptible}
	if t, err := time.Parse(time.RFC3339, gi.CreationTimestamp); err == nil {
		in.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, gi.LastStartTimestamp); err == nil && gi.Status == "RUNNING" {
		in.StartedAt = &t
	}
	for _, a := range gi.GuestAccelerators {
		in.GPU = fmt.Sprintf("%s:%d", path.Base(a.AcceleratorType), a.AcceleratorCount)
	}
	for _, d := range gi.Disks {
		if d.Boot {
			s := strings.Trim(string(d.DiskSizeGb), `"`)
			in.DiskGB, _ = strconv.Atoi(s)
		}
	}
	for _, it := range gi.Metadata.Items {
		switch it.Key {
		case MetaNodeID:
			in.NodeID = it.Value
		case MetaIdleMinutes:
			in.IdleMinutes = it.Value
		case MetaEnrollSecret, MetaTSAuthKey:
			in.SecretsPending = true
		}
	}
	return in
}

// Describe returns the instance, or nil when it does not exist.
func (g *GCP) Describe(ctx context.Context, zone, name string) (*Instance, error) {
	out, err := g.run(ctx, "compute", "instances", "describe", name, "--zone="+zone, "--format=json")
	if err != nil {
		if NotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var gi gceInstance
	if err := json.Unmarshal(out, &gi); err != nil {
		return nil, fmt.Errorf("gcloud instances describe: %w", err)
	}
	in := gi.instance()
	return &in, nil
}

// List returns every Pyre-managed instance in the project (all zones).
func (g *GCP) List(ctx context.Context) ([]Instance, error) {
	out, err := g.run(ctx, "compute", "instances", "list", "--filter=labels."+LabelManaged+"=true", "--format=json")
	if err != nil {
		return nil, err
	}
	var gis []gceInstance
	if err := json.Unmarshal(out, &gis); err != nil {
		return nil, fmt.Errorf("gcloud instances list: %w", err)
	}
	res := make([]Instance, 0, len(gis))
	for _, gi := range gis {
		res = append(res, gi.instance())
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res, nil
}

// CreateSpec is one instance to create.
type CreateSpec struct {
	Name, Zone, MachineType   string
	ImageFamily, ImageProject string
	DiskGB                    int
	DiskType                  string
	Spot                      bool
	Accelerator               string // "type=nvidia-tesla-t4,count=1" for N1; "" when bundled (G2/A2)
	GPU                       bool   // any GPU: maintenance policy TERMINATE
	Labels                    map[string]string
	Metadata                  map[string]string // non-secret values only
	MetadataFiles             map[string]string // key -> file (startup script, config, secrets)
	ServiceAccount            string
	StorageReadScope          bool // attach the SA with devstorage.read_only; else no SA at all
	NetworkTags               []string
	Network                   string
}

func joinKV(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// CreateArgs is the exact gcloud command line (tests assert it never holds
// a secret).
func (s CreateSpec) CreateArgs() []string {
	a := []string{"compute", "instances", "create", s.Name, "--zone=" + s.Zone, "--machine-type=" + s.MachineType,
		"--image-family=" + s.ImageFamily, "--image-project=" + s.ImageProject,
		fmt.Sprintf("--boot-disk-size=%dGB", s.DiskGB), "--boot-disk-type=" + s.DiskType}
	if s.Network != "" {
		a = append(a, "--network="+s.Network)
	}
	if len(s.Labels) > 0 {
		a = append(a, "--labels="+joinKV(s.Labels))
	}
	if len(s.NetworkTags) > 0 {
		a = append(a, "--tags="+strings.Join(s.NetworkTags, ","))
	}
	if len(s.Metadata) > 0 {
		a = append(a, "--metadata="+joinKV(s.Metadata))
	}
	if len(s.MetadataFiles) > 0 {
		a = append(a, "--metadata-from-file="+joinKV(s.MetadataFiles))
	}
	if s.Spot {
		a = append(a, "--provisioning-model=SPOT", "--instance-termination-action=STOP")
	}
	if s.Accelerator != "" {
		a = append(a, "--accelerator="+s.Accelerator)
	}
	if s.GPU {
		a = append(a, "--maintenance-policy=TERMINATE")
	}
	if s.StorageReadScope {
		if s.ServiceAccount != "" {
			a = append(a, "--service-account="+s.ServiceAccount)
		}
		a = append(a, "--scopes=https://www.googleapis.com/auth/devstorage.read_only")
	} else {
		a = append(a, "--no-service-account", "--no-scopes")
	}
	return append(a, "--format=json")
}

func (g *GCP) Create(ctx context.Context, s CreateSpec) error {
	_, err := g.run(ctx, s.CreateArgs()...)
	return err
}

func (g *GCP) Start(ctx context.Context, zone, name string) error {
	_, err := g.run(ctx, "compute", "instances", "start", name, "--zone="+zone)
	return err
}

func (g *GCP) Stop(ctx context.Context, zone, name string) error {
	_, err := g.run(ctx, "compute", "instances", "stop", name, "--zone="+zone)
	return err
}

// Delete removes the instance and every disk attached to it.
func (g *GCP) Delete(ctx context.Context, zone, name string) error {
	_, err := g.run(ctx, "compute", "instances", "delete", name, "--zone="+zone, "--delete-disks=all")
	return err
}

func (g *GCP) AddMetadata(ctx context.Context, zone, name string, kv map[string]string) error {
	_, err := g.run(ctx, "compute", "instances", "add-metadata", name, "--zone="+zone, "--metadata="+joinKV(kv))
	return err
}

func (g *GCP) AddMetadataFiles(ctx context.Context, zone, name string, files map[string]string) error {
	_, err := g.run(ctx, "compute", "instances", "add-metadata", name, "--zone="+zone, "--metadata-from-file="+joinKV(files))
	return err
}

func (g *GCP) RemoveMetadata(ctx context.Context, zone, name string, keys []string) error {
	_, err := g.run(ctx, "compute", "instances", "remove-metadata", name, "--zone="+zone, "--keys="+strings.Join(keys, ","))
	return err
}

func (g *GCP) AddDiskLabels(ctx context.Context, zone, disk string, labels map[string]string) error {
	_, err := g.run(ctx, "compute", "disks", "add-labels", disk, "--zone="+zone, "--labels="+joinKV(labels))
	return err
}

// Disk is a persistent disk.
type Disk struct {
	Name  string   `json:"name"`
	Zone  string   `json:"zone"`
	Users []string `json:"users"`
}

// DisksLabeled lists the disks carrying label=value (any zone).
func (g *GCP) DisksLabeled(ctx context.Context, label, value string) ([]Disk, error) {
	out, err := g.run(ctx, "compute", "disks", "list", "--filter=labels."+label+"="+value, "--format=json")
	if err != nil {
		return nil, err
	}
	var ds []Disk
	if err := json.Unmarshal(out, &ds); err != nil {
		return nil, fmt.Errorf("gcloud disks list: %w", err)
	}
	for i := range ds {
		ds[i].Zone = path.Base(ds[i].Zone)
	}
	return ds, nil
}

func (g *GCP) DeleteDisk(ctx context.Context, zone, name string) error {
	_, err := g.run(ctx, "compute", "disks", "delete", name, "--zone="+zone)
	return err
}

// ImageFamilyExists reports whether project has an image in family.
func (g *GCP) ImageFamilyExists(ctx context.Context, project, family string) (bool, error) {
	_, err := g.Run.Run(ctx, "compute", "images", "describe-from-family", family, "--project="+project, "--format=json", "--quiet")
	if err != nil {
		if NotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (g *GCP) CreateImage(ctx context.Context, name, family, sourceDisk, zone string, labels map[string]string) error {
	_, err := g.run(ctx, "compute", "images", "create", name, "--family="+family, "--source-disk="+sourceDisk,
		"--source-disk-zone="+zone, "--labels="+joinKV(labels), "--force")
	return err
}

// EnsureFirewall creates the deny-all-ingress rule for Pyre nodes when it
// is missing. Firewall rules cannot carry labels; the name and description
// mark it. Return traffic of outbound connections (Tailscale, apt, the hub
// over the tailnet) is unaffected: GCE firewalls are stateful.
func (g *GCP) EnsureFirewall(ctx context.Context, network string) (created bool, err error) {
	_, err = g.run(ctx, "compute", "firewall-rules", "describe", FirewallRule, "--format=json")
	if err == nil {
		return false, nil
	}
	if !NotFound(err) {
		return false, err
	}
	_, err = g.run(ctx, "compute", "firewall-rules", "create", FirewallRule, "--network="+network,
		"--direction=INGRESS", "--action=DENY", "--rules=all", "--source-ranges=0.0.0.0/0",
		"--target-tags="+NetworkTag, "--priority=100",
		"--description=pyre-managed: no inbound traffic to Pyre cloud nodes (reached over Tailscale only)")
	return err == nil, err
}

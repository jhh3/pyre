package cloud

import (
	"strconv"
	"strings"
)

// Cost estimates are rough on-demand list prices (us-central1/us-west1,
// USD) so the owner sees the order of magnitude; the bill is authoritative.
// Spot prices float daily: SpotFactor is a typical discount. Config
// "prices" overrides any entry.

// SpotFactor approximates spot price / on-demand price.
const SpotFactor = 0.35

// per vCPU-hour and per GB-hour of memory, by machine family.
var familyRates = map[string][2]float64{
	"e2":  {0.021811, 0.002923},
	"n2":  {0.031611, 0.004237},
	"n2d": {0.027502, 0.003686},
	"n4":  {0.030964, 0.004150},
	"c3":  {0.033982, 0.004555},
	"c3d": {0.029563, 0.003962},
	"c4":  {0.035550, 0.004010},
	"t2d": {0.027502, 0.003686},
	"t2a": {0.030800, 0.003850},
	"n1":  {0.031611, 0.004237},
}

// Machine types with bundled GPUs, whole-VM $/hour.
var bundledTypes = map[string]float64{
	"g2-standard-4": 0.7068, "g2-standard-8": 0.8536, "g2-standard-12": 1.0004, "g2-standard-16": 1.1472,
	"g2-standard-24": 2.0008, "g2-standard-32": 1.7340, "g2-standard-48": 4.0017, "g2-standard-96": 8.0034,
	"a2-highgpu-1g": 3.6730, "a2-highgpu-2g": 7.3460, "a2-highgpu-4g": 14.6920,
}

// GPU add-ons for N1, $/hour each.
var gpuRates = map[string]float64{
	"nvidia-tesla-t4": 0.35, "nvidia-tesla-p4": 0.60, "nvidia-tesla-v100": 2.48, "nvidia-tesla-p100": 1.46,
}

// vwsLicensePerGPU approximates the NVIDIA RTX Virtual Workstation license
// ($/GPU-hour) on Windows GPU nodes; "gpu-license:vws" in prices overrides it.
const vwsLicensePerGPU = 0.20

// Disk $/GB-month.
var diskRates = map[string]float64{"pd-balanced": 0.10, "pd-ssd": 0.17, "pd-standard": 0.04, "hyperdisk-balanced": 0.08}

// shared-core E2 types: vCPU (fractional billing is folded in), memory GB.
var sharedCore = map[string][2]float64{
	"e2-micro": {0.25, 1}, "e2-small": {0.5, 2}, "e2-medium": {1, 4},
}

// Estimate is an hourly cost estimate.
type Estimate struct {
	ComputeHourly float64 `json:"compute_hourly_usd"` // while RUNNING
	DiskHourly    float64 `json:"disk_hourly_usd"`    // always, until deleted
	Known         bool    `json:"known"`              // false: machine type not in the table
}

// Hourly is the cost per hour in the given state.
func (e Estimate) Hourly(running bool) float64 {
	if running {
		return e.ComputeHourly + e.DiskHourly
	}
	return e.DiskHourly
}

// EstimateCost estimates a machine. gpu is "type:count" or "".
func EstimateCost(machineType, gpu string, spot bool, diskGB int, diskType string, overrides map[string]float64) Estimate {
	var e Estimate
	if p, ok := overrides[machineType]; ok {
		e.ComputeHourly, e.Known = p, true
	} else if p, ok := bundledTypes[machineType]; ok {
		e.ComputeHourly, e.Known = p, true
	} else if vcpu, mem, fam, ok := shape(machineType); ok {
		if r, ok := familyRates[fam]; ok {
			e.ComputeHourly, e.Known = vcpu*r[0]+mem*r[1], true
		}
	}
	if g, n := splitGPU(gpu); strings.HasSuffix(g, "-vws") {
		// NVIDIA RTX Virtual Workstation license, per GPU-hour (also on G2).
		rate, ok := overrides["gpu-license:vws"]
		if !ok {
			rate = vwsLicensePerGPU
		}
		e.ComputeHourly += rate * float64(n)
	}
	if g, n := splitGPU(gpu); g != "" && !strings.HasPrefix(machineType, "g2-") && !strings.HasPrefix(machineType, "a2-") {
		g = strings.TrimSuffix(g, "-vws")
		rate, ok := overrides["gpu:"+g]
		if !ok {
			rate, ok = gpuRates[g]
		}
		if ok {
			e.ComputeHourly += rate * float64(n)
		} else {
			e.Known = false
		}
	}
	if spot {
		e.ComputeHourly *= SpotFactor
	}
	if diskType == "" {
		diskType = DefaultDiskType
	}
	rate, ok := overrides["disk:"+diskType]
	if !ok {
		rate = diskRates[diskType]
	}
	e.DiskHourly = rate * float64(diskGB) / 730
	return e
}

// shape parses "<family>-<class>-<vcpus>" (standard 4 GB/vCPU, highmem 8,
// highcpu 1; n1 standard 3.75, highmem 6.5, highcpu 0.9).
func shape(mt string) (vcpu, memGB float64, family string, ok bool) {
	if s, ok := sharedCore[mt]; ok {
		return s[0], s[1], "e2", true
	}
	parts := strings.Split(mt, "-")
	if len(parts) != 3 {
		return 0, 0, "", false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n <= 0 {
		return 0, 0, "", false
	}
	per := map[string]float64{"standard": 4, "highmem": 8, "highcpu": 1}
	if parts[0] == "n1" {
		per = map[string]float64{"standard": 3.75, "highmem": 6.5, "highcpu": 0.9}
	}
	m, ok := per[parts[1]]
	if !ok {
		return 0, 0, "", false
	}
	return float64(n), float64(n) * m, parts[0], true
}

func splitGPU(gpu string) (string, int) {
	if gpu == "" {
		return "", 0
	}
	t, c, found := strings.Cut(gpu, ":")
	n := 1
	if found {
		if v, err := strconv.Atoi(c); err == nil && v > 0 {
			n = v
		}
	}
	return t, n
}

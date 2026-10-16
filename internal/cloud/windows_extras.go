package cloud

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// WindowsInstaller is one owner-supplied installer a Windows node runs
// silently (gcp.windows_installers): the boot script downloads it from the
// owner's private bucket with the VM's read-only storage identity, checks its
// SHA-256 and runs it once (a marker per name and hash). Pyre never fetches
// vendor installers that sit behind registration forms itself: the owner
// uploads them.
type WindowsInstaller struct {
	Name   string   `json:"name"`
	GCSURI string   `json:"gcs_uri"` // gs://bucket/path/Setup.exe (.exe or .msi)
	SHA256 string   `json:"sha256"`
	Args   []string `json:"args,omitempty"` // silent-install arguments, e.g. ["/i", "/q", "/noreboot"]
}

var (
	installerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sha256Re        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	gcsURIRe        = regexp.MustCompile(`^gs://[a-z0-9][a-z0-9._-]{1,220}/[^\s]+$`)
)

// validateInstallers checks the list and returns it normalized (hash in
// lower case).
func validateInstallers(list []WindowsInstaller) ([]WindowsInstaller, error) {
	seen := map[string]bool{}
	out := make([]WindowsInstaller, 0, len(list))
	for _, in := range list {
		in.SHA256 = strings.ToLower(strings.TrimSpace(in.SHA256))
		switch {
		case !installerNameRe.MatchString(in.Name):
			return nil, contracts.Errorf(contracts.ErrInvalid, "windows_installers: name %q: use letters, digits, dot, dash, underscore", in.Name)
		case seen[in.Name]:
			return nil, contracts.Errorf(contracts.ErrInvalid, "windows_installers: name %q appears twice", in.Name)
		case !gcsURIRe.MatchString(in.GCSURI):
			return nil, contracts.Errorf(contracts.ErrInvalid, "windows_installers %s: gcs_uri must be gs://<bucket>/<object> (the project's private bucket)", in.Name)
		case !sha256Re.MatchString(in.SHA256):
			return nil, contracts.Errorf(contracts.ErrInvalid, "windows_installers %s: sha256 must be the 64-hex-digit SHA-256 of the file", in.Name)
		}
		lower := strings.ToLower(in.GCSURI)
		if !strings.HasSuffix(lower, ".exe") && !strings.HasSuffix(lower, ".msi") {
			return nil, contracts.Errorf(contracts.ErrInvalid, "windows_installers %s: the object must be an .exe or .msi", in.Name)
		}
		seen[in.Name] = true
		out = append(out, in)
	}
	return out, nil
}

// NVIDIA RTX Virtual Workstation (vWS): Windows GPU nodes attach the -vws
// accelerator variant, which carries the GRID license the Windows graphics
// driver needs (Compute Engine "virtual workstation" GPUs).
var vwsGPUs = map[string]bool{"nvidia-l4": true, "nvidia-tesla-t4": true, "nvidia-tesla-p4": true, "nvidia-tesla-p100": true}

// windowsGPU maps --gpu for a Windows node to its vWS accelerator: the GPU
// to size the machine with (without -vws), the vWS GPU ("nvidia-l4-vws:1")
// and the --accelerator value (attached explicitly, also on G2).
func windowsGPU(gpu string) (base, vws, accel string, err error) {
	g, n := splitGPU(gpu)
	g = strings.TrimSuffix(g, "-vws")
	if !vwsGPUs[g] {
		return "", "", "", contracts.Errorf(contracts.ErrInvalid, "Windows GPU nodes use NVIDIA RTX Virtual Workstation GPUs (nvidia-l4, nvidia-tesla-t4, nvidia-tesla-p4, nvidia-tesla-p100), not %s", g)
	}
	return fmt.Sprintf("%s:%d", g, n), fmt.Sprintf("%s-vws:%d", g, n), fmt.Sprintf("type=%s-vws,count=%d", g, n), nil
}

// DefaultGPUDriverBucket is Google's public bucket of NVIDIA drivers for
// Compute Engine; the Windows script picks the newest GRID (vWS) driver for
// Windows Server from it unless gcp.windows_gpu_driver names one.
const DefaultGPUDriverBucket = "nvidia-drivers-us-public"

func validateGPUDriver(u string) error {
	if u == "" || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "gs://"+DefaultGPUDriverBucket+"/") {
		return nil
	}
	return contracts.Errorf(contracts.ErrInvalid, "windows_gpu_driver must be https://… or gs://%s/GRID/… (read anonymously)", DefaultGPUDriverBucket)
}

package cagefile

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type BundleManifest struct {
	Name       string            `json:"name"`
	Tag        string            `json:"tag"`
	PackedWith string            `json:"packed_with"`
	Runtime    string            `json:"runtime"`
	Entrypoint string            `json:"entrypoint"`
	SystemDeps []string          `json:"system_deps,omitempty"`
	Packages   []string          `json:"packages,omitempty"`
	PipDeps    []string          `json:"pip_deps,omitempty"`
	NpmDeps    []string          `json:"npm_deps,omitempty"`
	GoDeps     []string          `json:"go_deps,omitempty"`
	EnvVars      map[string]string `json:"env,omitempty"`
	Capabilities AgentCapabilities `json:"capabilities"`
	FilesHash    string            `json:"files_hash"`
}

// SHA256 of the raw manifest.json bytes so a tampered manifest
// (e.g. injected pip dep) is rejected at unpack before any chroot
// install runs. Ed25519Sig and PublicKey are set when a signing key
// is provided at pack time, giving a proper trust anchor instead of
// a self-referencing hash.
type bundleSignature struct {
	ManifestHash string `json:"manifest_hash"`
	Ed25519Sig   string `json:"ed25519_sig,omitempty"`
	PublicKey    string `json:"public_key,omitempty"`
}

// PackOptions controls optional signing behavior. Nil means unsigned.
type PackOptions struct {
	SigningKey ed25519.PrivateKey
}

const bundleSignatureFile = "signature.json"

func Pack(dir string, version string, agentcageVersion string, w io.Writer, opts *PackOptions) (*BundleManifest, error) {
	cagefilePath := filepath.Join(dir, "Cagefile")
	f, err := os.Open(cagefilePath)
	if err != nil {
		return nil, fmt.Errorf("opening Cagefile in %s: %w", dir, err)
	}
	defer func() { _ = f.Close() }()

	manifest, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parsing Cagefile: %w", err)
	}

	if err := validateEntrypointExists(dir, manifest.Entrypoint); err != nil {
		return nil, err
	}

	hash, err := hashDir(dir)
	if err != nil {
		return nil, fmt.Errorf("hashing agent files: %w", err)
	}

	bundleManifest := &BundleManifest{
		Name:       filepath.Base(dir),
		Tag:        version,
		PackedWith: agentcageVersion,
		Runtime:    manifest.Runtime,
		Entrypoint: manifest.Entrypoint,
		SystemDeps: manifest.SystemDeps,
		Packages:   manifest.Packages,
		PipDeps:    manifest.PipDeps,
		NpmDeps:    manifest.NpmDeps,
		GoDeps:     manifest.GoDeps,
		EnvVars:      manifest.EnvVars,
		Capabilities: manifest.Capabilities,
		FilesHash:    "sha256:" + hash,
	}

	gw := gzip.NewWriter(w)
	defer func() { _ = gw.Close() }()

	tw := tar.NewWriter(gw)
	defer func() { _ = tw.Close() }()

	manifestBytes, err := json.MarshalIndent(bundleManifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling manifest: %w", err)
	}

	if err := writeToTar(tw, "manifest.json", manifestBytes); err != nil {
		return nil, fmt.Errorf("writing manifest to bundle: %w", err)
	}

	sig := bundleSignature{
		ManifestHash: "sha256:" + sha256Hex(manifestBytes),
	}
	if opts != nil && opts.SigningKey != nil {
		sigData := ed25519.Sign(opts.SigningKey, manifestBytes)
		sig.Ed25519Sig = hex.EncodeToString(sigData)
		pubBytes, pubErr := ExportPublicKeyPEM(opts.SigningKey)
		if pubErr != nil {
			return nil, fmt.Errorf("exporting public key: %w", pubErr)
		}
		sig.PublicKey = string(pubBytes)
	}
	sigBytes, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling bundle signature: %w", err)
	}
	if err := writeToTar(tw, bundleSignatureFile, sigBytes); err != nil {
		return nil, fmt.Errorf("writing signature to bundle: %w", err)
	}

	if err := addDirToTar(tw, dir, "files"); err != nil {
		return nil, fmt.Errorf("adding agent files to bundle: %w", err)
	}

	return bundleManifest, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const DefaultMaxBundleSize int64 = 2 * 1024 * 1024 * 1024

func PackToFile(dir, version, agentcageVersion, outPath string, maxSize int64, opts *PackOptions) (*BundleManifest, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxBundleSize
	}

	size, err := DirSize(dir)
	if err != nil {
		return nil, fmt.Errorf("calculating directory size: %w", err)
	}
	if size > maxSize {
		return nil, fmt.Errorf("directory is %.1f MB, exceeds max bundle size %.1f MB. Use --max-size to raise the limit",
			float64(size)/(1024*1024), float64(maxSize)/(1024*1024))
	}

	f, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("creating bundle file %s: %w", outPath, err)
	}
	defer func() { _ = f.Close() }()

	return Pack(dir, version, agentcageVersion, f, opts)
}

func DirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// UnpackOptions controls optional signature verification. Nil means
// accept bundles with or without Ed25519 signatures. When VerifyKey
// is set, an Ed25519 signature is required and must match.
type UnpackOptions struct {
	VerifyKey ed25519.PublicKey
}

// maxDecompressedSize bounds how much data Unpack writes to disk.
// 4x the compressed limit is generous for agent bundles; anything
// beyond that is a decompression bomb.
const maxDecompressedSize = DefaultMaxBundleSize * 4

func Unpack(r io.Reader, destDir string, opts *UnpackOptions) (*BundleManifest, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("opening gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	lr := &io.LimitedReader{R: gr, N: maxDecompressedSize}
	tr := tar.NewReader(lr)

	var (
		manifest          *BundleManifest
		manifestRawBytes  []byte
		signature         *bundleSignature
	)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar entry: %w", err)
		}

		target := filepath.Join(destDir, header.Name)

		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)+string(os.PathSeparator)) &&
			filepath.Clean(target) != filepath.Clean(destDir) {
			return nil, fmt.Errorf("invalid path in bundle: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return nil, fmt.Errorf("rejected unsafe entry in bundle: %s (type %d)", header.Name, header.Typeflag)

		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return nil, fmt.Errorf("creating directory %s: %w", target, err)
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, fmt.Errorf("creating parent for %s: %w", target, err)
			}

			// Mask permissions to prevent setuid/setgid/sticky bits and
			// limit to 0755 for executables, 0644 for everything else.
			mode := os.FileMode(header.Mode) & 0755
			if mode&0111 == 0 {
				mode = 0644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return nil, fmt.Errorf("creating file %s: %w", target, err)
			}

			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return nil, fmt.Errorf("extracting %s: %w", target, err)
			}
			_ = out.Close()

			// Raw bytes kept for signature verification below.
			if header.Name == "manifest.json" {
				data, readErr := os.ReadFile(target)
				if readErr != nil {
					return nil, fmt.Errorf("reading extracted manifest: %w", readErr)
				}
				manifestRawBytes = data
				manifest = &BundleManifest{}
				if jsonErr := json.Unmarshal(data, manifest); jsonErr != nil {
					return nil, fmt.Errorf("parsing manifest: %w", jsonErr)
				}
			}

			if header.Name == bundleSignatureFile {
				data, readErr := os.ReadFile(target)
				if readErr != nil {
					return nil, fmt.Errorf("reading bundle signature: %w", readErr)
				}
				signature = &bundleSignature{}
				if jsonErr := json.Unmarshal(data, signature); jsonErr != nil {
					return nil, fmt.Errorf("parsing bundle signature: %w", jsonErr)
				}
			}
		}
	}

	if manifest == nil {
		return nil, fmt.Errorf("bundle does not contain manifest.json")
	}

	if signature == nil {
		return nil, fmt.Errorf("bundle is missing signature.json, repack with a current agentcage version")
	}
	expected := "sha256:" + sha256Hex(manifestRawBytes)
	if signature.ManifestHash != expected {
		return nil, fmt.Errorf("bundle manifest hash mismatch, manifest may be tampered (expected %s, got %s)", signature.ManifestHash, expected)
	}

	requireSig := opts != nil && opts.VerifyKey != nil
	if requireSig && signature.Ed25519Sig == "" {
		return nil, fmt.Errorf("bundle is not signed with Ed25519 but verification key was provided")
	}
	if signature.Ed25519Sig != "" {
		sigBytes, decErr := hex.DecodeString(signature.Ed25519Sig)
		if decErr != nil {
			return nil, fmt.Errorf("decoding Ed25519 signature: %w", decErr)
		}
		var verifyKey ed25519.PublicKey
		if requireSig {
			verifyKey = opts.VerifyKey
		} else if signature.PublicKey != "" {
			parsed, parseErr := ParsePublicKeyPEM([]byte(signature.PublicKey))
			if parseErr != nil {
				return nil, fmt.Errorf("parsing embedded public key: %w", parseErr)
			}
			verifyKey = parsed
		}
		if verifyKey != nil && !ed25519.Verify(verifyKey, manifestRawBytes, sigBytes) {
			return nil, fmt.Errorf("Ed25519 signature verification failed, bundle may be tampered")
		}
	}

	if manifest.FilesHash != "" {
		filesDir := filepath.Join(destDir, "files")
		actualHash, hashErr := hashDir(filesDir)
		if hashErr != nil {
			return nil, fmt.Errorf("verifying bundle files hash: %w", hashErr)
		}
		if "sha256:"+actualHash != manifest.FilesHash {
			return nil, fmt.Errorf("bundle files hash mismatch: expected %s, got sha256:%s", manifest.FilesHash, actualHash)
		}
	}

	return manifest, nil
}

func CheckCompatibility(bundle *BundleManifest, currentVersion string) error {
	// PackedWith records the agentcage version that created the bundle.
	// Empty means pre-PackedWith bundle; skip check.
	if bundle.PackedWith == "" {
		return nil
	}
	bundleMajor, bundleMinor, err := majorMinorVersion(bundle.PackedWith)
	if err != nil {
		return fmt.Errorf("invalid bundle packed_with version %q: %w", bundle.PackedWith, err)
	}
	currentMajor, currentMinor, err := majorMinorVersion(currentVersion)
	if err != nil {
		return fmt.Errorf("invalid current version %q: %w", currentVersion, err)
	}
	if bundleMajor != currentMajor {
		return fmt.Errorf("bundle was packed with agentcage v%s (major %d) but this is v%s (major %d): major version mismatch",
			bundle.PackedWith, bundleMajor, currentVersion, currentMajor)
	}
	// Pre-1.0: minor bumps are breaking per semver. Patch differences
	// within the same minor are compatible (0.1.0 works with 0.1.5).
	if bundleMajor == 0 && bundleMinor != currentMinor {
		return fmt.Errorf("bundle was packed with agentcage v%s but this is v%s: minor version mismatch (pre-1.0 minors are breaking)",
			bundle.Tag, currentVersion)
	}
	return nil
}

// Packages that would give the agent capabilities beyond what the
// cage's containment model expects. Blocked at pack-time so the
// operator gets a clear error before any cage is provisioned.
var deniedSystemDeps = map[string]string{
	"docker":      "container runtime",
	"docker.io":   "container runtime",
	"containerd":  "container runtime",
	"podman":      "container runtime",
	"lxc":         "container runtime",
	"iptables":    "firewall manipulation",
	"nftables":    "firewall manipulation",
	"tcpdump":     "raw packet capture",
	"wireshark":   "raw packet capture",
	"tshark":      "raw packet capture",
	"socat":       "raw socket relay",
	"openvpn":     "VPN tunnel",
	"wireguard":   "VPN tunnel",
	"kmod":        "kernel module tools",
	"linux-headers": "kernel headers",
}

// CheckContentPolicy rejects bundles that declare system
// dependencies known to break the cage containment model.
func CheckContentPolicy(manifest *BundleManifest) error {
	for _, dep := range manifest.SystemDeps {
		if reason, denied := deniedSystemDeps[dep]; denied {
			return fmt.Errorf("system dependency %q is denied (%s)", dep, reason)
		}
	}
	return nil
}

func majorMinorVersion(v string) (int, int, error) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	if len(parts) < 2 {
		return major, 0, nil
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

func UnpackFile(bundlePath, destDir string) (*BundleManifest, error) {
	return UnpackFileWithOpts(bundlePath, destDir, nil)
}

func UnpackFileWithOpts(bundlePath, destDir string, opts *UnpackOptions) (*BundleManifest, error) {
	f, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("opening bundle %s: %w", bundlePath, err)
	}
	defer func() { _ = f.Close() }()
	return Unpack(f, destDir, opts)
}

func writeToTar(tw *tar.Writer, name string, data []byte) error {
	header := &tar.Header{
		Name:    name,
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func addDirToTar(tw *tar.Writer, srcDir, prefix string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Represented by manifest.json in the bundle.
		if info.Name() == "Cagefile" && filepath.Dir(path) == srcDir {
			return nil
		}

		// Resolve symlinks but reject any that escape the agent directory.
		linfo, lstatErr := os.Lstat(path)
		if lstatErr != nil {
			return fmt.Errorf("stat %s: %w", path, lstatErr)
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				return fmt.Errorf("resolving symlink %s: %w", path, resolveErr)
			}
			if !strings.HasPrefix(resolved, srcDir) {
				return fmt.Errorf("symlink %s escapes agent directory (points to %s)", path, resolved)
			}
			// Skip directory symlinks to avoid walking them twice.
			resolvedInfo, statErr := os.Stat(resolved)
			if statErr != nil {
				return fmt.Errorf("stat resolved symlink %s: %w", resolved, statErr)
			}
			if resolvedInfo.IsDir() {
				return filepath.SkipDir
			}
			// Use the resolved file's info for correct size in tar header.
			info = resolvedInfo
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		tarPath := filepath.Join(prefix, rel)
		if tarPath == prefix {
			return nil
		}

		if info.IsDir() {
			header := &tar.Header{
				Name:     tarPath + "/",
				Typeflag: tar.TypeDir,
				Mode:     0755,
				ModTime:  info.ModTime(),
			}
			return tw.WriteHeader(header)
		}

		header := &tar.Header{
			Name:    tarPath,
			Size:    info.Size(),
			Mode:    int64(info.Mode()),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		_, copyErr := io.Copy(tw, file)
		_ = file.Close()
		return copyErr
	})
}

func HashDir(dir string) (string, error) {
	return hashDir(dir)
}

func hashDir(dir string) (string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if info.Name() == "Cagefile" && filepath.Dir(path) == dir {
			return nil
		}
		linfo, lstatErr := os.Lstat(path)
		if lstatErr != nil {
			return fmt.Errorf("stat %s: %w", path, lstatErr)
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				return fmt.Errorf("resolving symlink %s: %w", path, resolveErr)
			}
			if !strings.HasPrefix(resolved, dir) {
				return fmt.Errorf("symlink %s escapes agent directory (points to %s)", path, resolved)
			}
			// Skip directory symlinks (same as tar logic).
			resolvedInfo, statErr := os.Stat(resolved)
			if statErr != nil || resolvedInfo.IsDir() {
				return nil
			}
			// Include resolved file symlinks in hash to match
			// what gets bundled (symlinks are followed in the tar).
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Strings(files)

	h := sha256.New()
	for _, rel := range files {
		// Normalize path separators so the hash is identical across platforms
		h.Write([]byte(filepath.ToSlash(rel)))
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", err
		}
		h.Write(data)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// Interpreters resolved via PATH inside the cage, not from the
// agent directory.
var knownInterpreters = map[string]bool{
	"python3": true,
	"python":  true,
	"node":    true,
	"bash":    true,
	"sh":      true,
	"go":      true,
}

func validateEntrypointExists(dir, entrypoint string) error {
	parts := strings.Fields(entrypoint)
	if len(parts) == 0 {
		return fmt.Errorf("entrypoint is empty")
	}

	// Direct executable: ./run.sh, mybinary
	if len(parts) == 1 {
		return checkAgentFile(dir, parts[0])
	}

	first := parts[0]
	args := parts[1:]

	// Not a known interpreter; first token is the executable itself.
	if !knownInterpreters[filepath.Base(first)] {
		return checkAgentFile(dir, first)
	}

	// python3 -m module — module lives in the agent dir as module.py
	// or module/ (package with __init__.py).
	for i, arg := range args {
		if arg == "-m" && i+1 < len(args) {
			mod := args[i+1]
			if fileExists(filepath.Join(dir, mod+".py")) || fileExists(filepath.Join(dir, mod)) {
				return nil
			}
			return fmt.Errorf("entrypoint module %q not found in %s (checked %s.py and %s/)", mod, dir, mod, mod)
		}
	}

	// Skip interpreter flags to find the script/file argument.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		// Absolute paths resolve inside the cage, not the agent dir.
		if filepath.IsAbs(arg) {
			return nil
		}
		return checkAgentFile(dir, arg)
	}

	// All args are flags (e.g. "python3 --version"). Nothing to check.
	return nil
}

func checkAgentFile(dir, name string) error {
	clean := filepath.Clean(name)
	// Absolute paths resolve inside the cage at runtime.
	if filepath.IsAbs(clean) {
		return nil
	}
	path := filepath.Join(dir, clean)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("entrypoint %q not found in %s", clean, dir)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

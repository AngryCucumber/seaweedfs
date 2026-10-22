// Command pkginfo supports the MassDB OSS installation package build
// (package/build/massdb-package.sh). It checks trust manifests with the same
// parser as the weed binary and writes package.json.
//
//	pkginfo version                       code version, e.g. 3.84
//	pkginfo trust-b64 <file>              check a test trust manifest (JSON or
//	                                      its base64url form) and print the
//	                                      base64url string for -ldflags
//	pkginfo package-json -commit C -arch A [-large-disk] [-trust-b64 S]
//	                                      print package.json
//	pkginfo check-binary <weed> <package.json>
//	                                      the trust recorded in package.json
//	                                      must appear verbatim in the binary
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// packageInfo is package.json. The platform's package inspector reads it, so
// field names and meanings only change together with the inspector.
type packageInfo struct {
	SchemaVersion    int      `json:"schemaVersion"`
	Kind             string   `json:"kind"`
	Product          string   `json:"product"`
	Version          string   `json:"version"`
	Commit           string   `json:"commit"`
	OS               string   `json:"os"`
	Arch             string   `json:"arch"`
	LargeDisk        bool     `json:"largeDisk"`
	LicenseSupported bool     `json:"licenseSupported"`
	Trust            string   `json:"trust"`
	TrustEncoding    string   `json:"trustEncoding"`
	Roles            []string `json:"roles"`
}

const (
	trustEmbeddedJSON     = "embedded-json"
	trustLdflagsBase64URL = "ldflags-base64url"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: pkginfo version|trust-b64|package-json|check-binary ...")
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(util.VERSION_NUMBER)
	case "trust-b64":
		err = trustBase64(os.Args[2:])
	case "package-json":
		err = packageJSON(os.Args[2:])
	case "check-binary":
		err = checkBinary(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "pkginfo:", msg)
	os.Exit(1)
}

// trustBase64 accepts the issuer's trust manifest as JSON or as the base64url
// string of it, checks that the binary would accept it next to the production
// manifest, and prints the base64url string.
func trustBase64(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: pkginfo trust-b64 <file>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	data = bytes.TrimSpace(data)
	manifest := data
	if !bytes.HasPrefix(data, []byte("{")) {
		if manifest, err = base64.RawURLEncoding.Strict().DecodeString(string(data)); err != nil {
			return fmt.Errorf("%s is neither JSON nor unpadded base64url: %v", args[0], err)
		}
	}
	trust, err := license.NewTrustStore([]byte(license.ProductionTrustManifest()), manifest)
	if err != nil {
		return fmt.Errorf("%s: %v", args[0], err)
	}
	if !trust.IsTest() {
		return fmt.Errorf("%s: empty trust manifest", args[0])
	}
	fmt.Println(base64.RawURLEncoding.EncodeToString(manifest))
	return nil
}

func packageJSON(args []string) error {
	fs := flag.NewFlagSet("package-json", flag.ContinueOnError)
	commit := fs.String("commit", "", "commit injected as util.COMMIT")
	arch := fs.String("arch", "", "arm64 or amd64")
	largeDisk := fs.Bool("large-disk", false, "built with the 5BytesOffset tag")
	trustB64 := fs.String("trust-b64", "", "test trust manifest injected with -ldflags")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *commit == "" || (*arch != "arm64" && *arch != "amd64") {
		return fmt.Errorf("package-json needs -commit and -arch arm64|amd64")
	}
	info := packageInfo{
		SchemaVersion:    1,
		Kind:             "massdb-oss",
		Product:          license.Product,
		Version:          util.VERSION_NUMBER,
		Commit:           *commit,
		OS:               "linux",
		Arch:             *arch,
		LargeDisk:        *largeDisk,
		LicenseSupported: true,
		Roles:            []string{"master", "volume", "filer", "s3"},
	}
	production := license.ProductionTrustManifest()
	switch {
	case *trustB64 != "" && production != "":
		// package.json has room for one trust root; extend it before
		// building a test root on top of a production one.
		return fmt.Errorf("a test trust root on top of the non-empty production manifest is not supported by package.json yet")
	case *trustB64 != "":
		info.Trust, info.TrustEncoding = *trustB64, trustLdflagsBase64URL
	case production != "":
		info.Trust, info.TrustEncoding = production, trustEmbeddedJSON
	default:
		return fmt.Errorf("the production trust manifest in weed/license/trust.go is empty; pass a test trust root (TRUST=)")
	}
	out, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func checkBinary(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: pkginfo check-binary <weed> <package.json>")
	}
	binary, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	var info packageInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}
	if info.Trust == "" || !bytes.Contains(binary, []byte(info.Trust)) {
		return fmt.Errorf("the %s trust recorded in %s is not in %s", info.TrustEncoding, args[1], args[0])
	}
	if !bytes.Contains(binary, []byte(info.Commit)) || !bytes.Contains(binary, []byte("github.com/seaweedfs/seaweedfs/weed")) {
		return fmt.Errorf("%s does not carry commit %s", args[0], info.Commit)
	}
	return nil
}

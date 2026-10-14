package command

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func init() {
	cmdLicenseInit.Run = runLicenseInit // break init cycle
}

var cmdLicenseInit = &Command{
	UsageLine: "license.init -mdir=/data/master [-clusterId=<uuid>]",
	Short:     "create the license cluster_id of a master and print the license request",
	Long: `Offline step before deploying or upgrading masters with a license.

  Run it with the new binary while the master is stopped. It writes
  <mdir>/license/state.json with a cluster_id unless one exists, and prints
  the license request to send to the vendor. Masters then start with
  -license.file=<certificate>.

  With several masters, run it on one of them, then on every other one
  with -clusterId=<the printed deployment_id>. After losing <mdir>/license,
  restore the id with -clusterId=<deployment_id of the certificate>.
  An existing different cluster_id is never replaced.
`,
}

var (
	licenseInitMdir      = cmdLicenseInit.Flag.String("mdir", "", "the master -mdir")
	licenseInitClusterId = cmdLicenseInit.Flag.String("clusterId", "", "use this cluster_id instead of generating one")
)

func runLicenseInit(cmd *Command, args []string) bool {
	if *licenseInitMdir == "" || len(args) != 0 {
		return false
	}
	mdir := util.ResolvePath(*licenseInitMdir)
	req, err := license.InitState(mdir, *licenseInitClusterId)
	if err != nil {
		fmt.Fprintf(os.Stderr, "license.init: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.Marshal(req)
	fmt.Println(string(out))
	return true
}

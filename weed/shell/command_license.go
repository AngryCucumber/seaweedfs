package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

func init() {
	Commands = append(Commands, &commandLicenseShow{})
	Commands = append(Commands, &commandLicenseImport{validateOnly: false})
	Commands = append(Commands, &commandLicenseImport{validateOnly: true})
}

type commandLicenseShow struct {
}

func (c *commandLicenseShow) Name() string {
	return "license.show"
}

func (c *commandLicenseShow) Help() string {
	return `show the license status of the master

	license.show

	Prints the same document as GET /license on the master the shell is
	connected to. "voters_acked" lists the voters known to hold the current
	license state; check it before removing masters of a goraft cluster.
`
}

func (c *commandLicenseShow) HasTag(CommandTag) bool {
	return false
}

func (c *commandLicenseShow) Do(args []string, commandEnv *CommandEnv, writer io.Writer) error {
	return commandEnv.MasterClient.WithClient(false, func(client master_pb.SeaweedClient) error {
		resp, err := client.GetLicense(context.Background(), &master_pb.GetLicenseRequest{})
		if err != nil {
			return err
		}
		return printJson(writer, resp.StatusJson)
	})
}

type commandLicenseImport struct {
	validateOnly bool
}

func (c *commandLicenseImport) Name() string {
	if c.validateOnly {
		return "license.validate"
	}
	return "license.import"
}

func (c *commandLicenseImport) Help() string {
	if c.validateOnly {
		return `check a license certificate without importing it

	license.validate -file=<certificate file>

	Runs the same checks as license.import; nothing is stored.
`
	}
	return `import a license certificate on the leader master

	license.import -file=<certificate file>

	The leader verifies the certificate, stores it and commits it to a
	majority of the voting masters; volume servers and filers receive it
	with their next heartbeat. Importing the same file again only resends it.
`
}

func (c *commandLicenseImport) HasTag(CommandTag) bool {
	return false
}

func (c *commandLicenseImport) Do(args []string, commandEnv *CommandEnv, writer io.Writer) error {
	flags := flag.NewFlagSet(c.Name(), flag.ContinueOnError)
	file := flags.String("file", "", "license certificate file")
	if err := flags.Parse(args); err != nil {
		return nil
	}
	if *file == "" {
		return fmt.Errorf("-file is required")
	}
	info, err := os.Stat(*file)
	if err != nil {
		return err
	}
	if info.Size() > license.MaxRequestBytes {
		return fmt.Errorf("%s is larger than %d bytes", *file, license.MaxRequestBytes)
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	return commandEnv.MasterClient.WithClient(false, func(client master_pb.SeaweedClient) error {
		resp, err := client.ImportLicense(context.Background(), &master_pb.ImportLicenseRequest{
			Certificate:  string(bytes.TrimSpace(raw)),
			ValidateOnly: c.validateOnly,
		})
		if err != nil {
			return err
		}
		if err := printJson(writer, resp.ResultJson); err != nil {
			return err
		}
		return printLicenseChanges(writer, resp.ResultJson)
	})
}

// printLicenseChanges adds one line naming what the certificate takes away
// (the "changes" of the result), so they are not missed in the JSON.
func printLicenseChanges(writer io.Writer, doc string) error {
	var out struct {
		Changes []map[string]interface{} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(doc), &out); err != nil || len(out.Changes) == 0 {
		return nil
	}
	codes := make([]string, len(out.Changes))
	for i, c := range out.Changes {
		codes[i] = fmt.Sprint(c["code"])
	}
	_, err := fmt.Fprintf(writer, "changes: %s\n", strings.Join(codes, ", "))
	return err
}

func printJson(writer io.Writer, doc string) error {
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(doc), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err := writer.Write(out.Bytes())
	return err
}

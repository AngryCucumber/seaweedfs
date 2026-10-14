package master_ui

import (
	_ "embed"
	"html/template"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/license"
)

//go:embed master.html
var masterHtml string

//go:embed masterNewRaft.html
var masterNewRaftHtml string

//go:embed license.html
var licenseHtml string

// licenseWarnDays turns the expiry yellow this many days before.
const licenseWarnDays = 30

var licenseFunctions = template.FuncMap{
	"unixTime": func(sec int64) string {
		return time.Unix(sec, 0).UTC().Format("2006-01-02 15:04:05 UTC")
	},
	"licenseLabel": func(s *license.Status) string {
		switch s.Status {
		case license.ReasonValid:
			if s.DaysRemaining < licenseWarnDays {
				return "label-warning"
			}
			return "label-success"
		case license.ReasonReconciling:
			return "label-info"
		}
		return "label-danger"
	},
	"licenseExpiryClass": func(s *license.Status) string {
		switch {
		case s.Effective == nil:
			return ""
		case s.ExpiresAt <= s.Now:
			return "danger"
		case s.DaysRemaining < licenseWarnDays:
			return "warning"
		}
		return ""
	},
	"licensePending": func(s *license.Status) bool {
		return s.Newest != nil && (s.Effective == nil || s.Effective.Sequence != s.Newest.Sequence)
	},
}

var templateFunctions = template.FuncMap{
	"url": func(input string) string {

		if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") {
			return "http://" + input
		}

		return input
	},
}

var StatusTpl = template.Must(template.Must(template.New("status").Funcs(templateFunctions).Funcs(licenseFunctions).Parse(masterHtml)).Parse(licenseHtml))

var StatusNewRaftTpl = template.Must(template.Must(template.New("status").Funcs(licenseFunctions).Parse(masterNewRaftHtml)).Parse(licenseHtml))

package weed_server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/pb"
)

// The /license handlers never proxy to the leader: status is the local
// view, and import answers 503 NOT_LEADER with the leader address. Request
// bodies and responses never echo certificate text.

func (ms *MasterServer) licenseStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	writeJsonQuiet(w, r, http.StatusOK, ms.License.Status())
}

func (ms *MasterServer) licenseDeploymentHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	d, err := ms.License.Deployment()
	if err != nil {
		ms.writeLicenseError(w, r, err, nil)
		return
	}
	writeJsonQuiet(w, r, http.StatusOK, d)
}

func (ms *MasterServer) licenseValidateHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	raw, err := readLicenseCertificate(w, r)
	if err != nil {
		ms.writeLicenseError(w, r, err, nil)
		return
	}
	out, err := ms.License.Validate(raw)
	if err != nil {
		ms.writeLicenseError(w, r, err, nil)
		return
	}
	writeJsonQuiet(w, r, http.StatusOK, out)
}

func (ms *MasterServer) licenseImportHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	raw, err := readLicenseCertificate(w, r)
	if err != nil {
		ms.writeLicenseError(w, r, err, nil)
		return
	}
	out, err := ms.License.Import(raw)
	if err != nil {
		ms.writeLicenseError(w, r, err, out)
		return
	}
	writeJsonQuiet(w, r, http.StatusOK, out)
}

func (ms *MasterServer) licenseNodeHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodDelete) {
		return
	}
	addr := mux.Vars(r)["addr"]
	if err := ms.License.ReleaseNode(addr); err != nil {
		ms.writeLicenseError(w, r, err, nil)
		return
	}
	writeJsonQuiet(w, r, http.StatusOK, map[string]string{"released": addr})
}

func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeJsonQuiet(w, r, http.StatusMethodNotAllowed, map[string]string{"error": "METHOD_NOT_ALLOWED", "message": method + " only"})
	return false
}

// writeLicenseError writes {"error":<code>,"message":...} with the status of
// the code (contract 2.3), plus the leader for NOT_LEADER and the voter
// lists for PARTIAL_COMMIT.
func (ms *MasterServer) writeLicenseError(w http.ResponseWriter, r *http.Request, err error, out *license.ImportOutcome) {
	var le *license.Error
	if !errors.As(err, &le) {
		writeJsonQuiet(w, r, http.StatusInternalServerError, map[string]string{"error": "INTERNAL", "message": err.Error()})
		return
	}
	body := map[string]interface{}{"error": le.Code, "message": le.Message}
	switch le.Code {
	case license.CodeNotLeader:
		leader := ""
		if id := (masterRaft{ms}).Leader(); id != "" {
			leader = pb.ServerAddress(id).ToHttpAddress()
		}
		body["leader"] = leader
	case license.CodePartialCommit:
		if out != nil {
			body["voters_ok"] = out.VotersOK
			body["peers_failed"] = out.PeersFailed
			body["changes"] = out.Changes
		}
	}
	writeJsonQuiet(w, r, le.Code.HTTPStatus(), body)
}

// readLicenseCertificate accepts {"certificate":"<jws>"}, a multipart form
// with a "file" field, or the certificate itself as the body.
func readLicenseCertificate(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	tooLarge := &license.Error{Code: license.CodeRequestTooLarge, Message: fmt.Sprintf("request body is larger than %d bytes", license.MaxRequestBytes)}
	if r.ContentLength > license.MaxRequestBytes {
		return nil, tooLarge
	}
	r.Body = http.MaxBytesReader(w, r.Body, license.MaxRequestBytes)
	isTooLarge := func(err error) bool {
		var mbe *http.MaxBytesError
		return errors.As(err, &mbe)
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(license.MaxRequestBytes); err != nil {
			if isTooLarge(err) {
				return nil, tooLarge
			}
			return nil, &license.Error{Code: license.CodeMalformed, Message: "cannot parse the multipart form"}
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			return nil, &license.Error{Code: license.CodeMalformed, Message: `the multipart form has no "file" field`}
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, license.MaxRequestBytes))
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		if isTooLarge(err) {
			return nil, tooLarge
		}
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '{' {
		var req struct {
			Certificate string `json:"certificate"`
		}
		if err := json.Unmarshal(data, &req); err != nil || req.Certificate == "" {
			return nil, &license.Error{Code: license.CodeMalformed, Message: `the request needs {"certificate":"<jws>"}`}
		}
		return []byte(req.Certificate), nil
	}
	if len(data) == 0 {
		return nil, &license.Error{Code: license.CodeMalformed, Message: "the request has no certificate"}
	}
	return data, nil
}

package weed_server

import (
	"context"
	"encoding/json"

	"github.com/seaweedfs/seaweedfs/weed/license"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

func (ms *MasterServer) GetLicense(ctx context.Context, req *master_pb.GetLicenseRequest) (*master_pb.GetLicenseResponse, error) {
	resp := &master_pb.GetLicenseResponse{}
	if req.IncludeState {
		if err := ms.checkLicensePeer(ctx); err != nil {
			return nil, err
		}
		resp.State = licenseStateToPb(ms.License.State())
		return resp, nil
	}
	status, err := json.Marshal(ms.License.Status())
	if err != nil {
		return nil, err
	}
	resp.StatusJson = string(status)
	return resp, nil
}

func (ms *MasterServer) ImportLicense(ctx context.Context, req *master_pb.ImportLicenseRequest) (*master_pb.ImportLicenseResponse, error) {
	if len(req.Certificate) > license.MaxRequestBytes {
		return nil, licenseGrpcError(&license.Error{Code: license.CodeRequestTooLarge})
	}
	var out interface{}
	var err error
	if req.ValidateOnly {
		out, err = ms.License.Validate([]byte(req.Certificate))
	} else {
		out, err = ms.License.Import([]byte(req.Certificate))
	}
	if err != nil {
		return nil, licenseGrpcError(err)
	}
	result, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &master_pb.ImportLicenseResponse{ResultJson: string(result)}, nil
}

func (ms *MasterServer) SyncLicense(ctx context.Context, req *master_pb.SyncLicenseRequest) (*master_pb.SyncLicenseResponse, error) {
	if err := ms.checkLicensePeer(ctx); err != nil {
		return nil, err
	}
	st, err := ms.License.ApplySync(license.LeaderInfo{
		Address: req.LeaderAddress,
		Term:    req.LeaderTerm,
	}, licenseStateFromPb(req.State))
	if err != nil {
		return nil, licenseGrpcError(err)
	}
	return &master_pb.SyncLicenseResponse{State: licenseStateToPb(st)}, nil
}

func (ms *MasterServer) GetLedger(ctx context.Context, req *master_pb.GetLedgerRequest) (*master_pb.GetLedgerResponse, error) {
	if err := ms.checkLicensePeer(ctx); err != nil {
		return nil, err
	}
	return &master_pb.GetLedgerResponse{Ledger: ledgerToPb(ms.License.Ledger())}, nil
}

func (ms *MasterServer) SyncLedger(ctx context.Context, req *master_pb.SyncLedgerRequest) (*master_pb.SyncLedgerResponse, error) {
	if err := ms.checkLicensePeer(ctx); err != nil {
		return nil, err
	}
	err := ms.License.ApplyLedgerSync(license.LeaderInfo{
		Address: req.LeaderAddress,
		Term:    req.LeaderTerm,
	}, ledgerFromPb(req.Ledger))
	if err != nil {
		return nil, licenseGrpcError(err)
	}
	return &master_pb.SyncLedgerResponse{}, nil
}

package warehouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	proto "github.com/rudderlabs/rudder-server/proto/warehouse"
	"github.com/rudderlabs/rudder-server/warehouse/validations"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type warehouseGRPC struct {
	proto.UnimplementedWarehouseServer
}

func (*warehouseGRPC) GetWHUploads(_ context.Context, request *proto.WHUploadsRequest) (*proto.WHUploadsResponse, error) {
	uploadsReq := UploadsReqT{
		WorkspaceID:     request.WorkspaceId,
		SourceID:        request.SourceId,
		DestinationID:   request.DestinationId,
		DestinationType: request.DestinationType,
		Status:          request.Status,
		Limit:           request.Limit,
		Offset:          request.Offset,
		API:             UploadAPI,
	}
	res, err := uploadsReq.GetWhUploads()
	return res, err
}

func (*warehouseGRPC) TriggerWHUploads(_ context.Context, request *proto.WHUploadsRequest) (*proto.TriggerWhUploadsResponse, error) {
	uploadsReq := UploadsReqT{
		WorkspaceID:   request.WorkspaceId,
		SourceID:      request.SourceId,
		DestinationID: request.DestinationId,
		API:           UploadAPI,
	}
	res, err := uploadsReq.TriggerWhUploads()
	return res, err
}

func (*warehouseGRPC) GetWHUpload(_ context.Context, request *proto.WHUploadRequest) (*proto.WHUploadResponse, error) {
	uploadReq := UploadReqT{
		UploadId:    request.UploadId,
		WorkspaceID: request.WorkspaceId,
		API:         UploadAPI,
	}
	res, err := uploadReq.GetWHUpload()
	return res, err
}

func (*warehouseGRPC) GetHealth(context.Context, *emptypb.Empty) (*wrapperspb.BoolValue, error) {
	return wrapperspb.Bool(UploadAPI.enabled), nil
}

func (*warehouseGRPC) TriggerWHUpload(_ context.Context, request *proto.WHUploadRequest) (*proto.TriggerWhUploadsResponse, error) {
	uploadReq := UploadReqT{
		UploadId:    request.UploadId,
		WorkspaceID: request.WorkspaceId,
		API:         UploadAPI,
	}
	res, err := uploadReq.TriggerWHUpload()
	return res, err
}

func (*warehouseGRPC) Validate(_ context.Context, req *proto.WHValidationRequest) (*proto.WHValidationResponse, error) {
	handleT := validations.CTHandleT{}
	return handleT.Validating(req)
}

func (*warehouseGRPC) RetryWHUploads(ctx context.Context, req *proto.RetryWHUploadsRequest) (response *proto.RetryWHUploadsResponse, err error) {
	retryReq := &RetryRequest{
		WorkspaceID:     req.WorkspaceId,
		SourceID:        req.SourceId,
		DestinationID:   req.DestinationId,
		DestinationType: req.DestinationType,
		IntervalInHours: req.IntervalInHours,
		ForceRetry:      req.ForceRetry,
		UploadIds:       req.UploadIds,
		API:             UploadAPI,
	}
	r, err := retryReq.RetryWHUploads(ctx)
	response = &proto.RetryWHUploadsResponse{
		Message:    r.Message,
		StatusCode: r.StatusCode,
	}
	return
}

type ObjectStorageValidationRequest struct {
	Type   string                 `json:"type"`
	Config map[string]interface{} `json:"config"`
}

func validateObjectStorageRequestBody(request *proto.ValidateObjectStorageRequest) (*ObjectStorageValidationRequest, error) {
	byt, err := json.Marshal(request)
	if err != nil {
		return nil, status.Errorf(
			codes.Code(code.Code_INTERNAL),
			"unable to marshal the request proto message with error: %s", err.Error())
	}

	r := &ObjectStorageValidationRequest{}
	if err := json.Unmarshal(byt, r); err != nil {
		return nil, status.Errorf(
			codes.Code(code.Code_INTERNAL),
			"unable to extract data into validation request with error: %s", err)
	}
	switch r.Type {
	case "AZURE_BLOB":
		if !checkMapForValidKey(r.Config, "containerName") {
			err = fmt.Errorf("containerName invalid or not present")
		}
	case "GCS", "MINIO", "S3", "DIGITAL_OCEAN_SPACES":
		if !checkMapForValidKey(r.Config, "bucketName") {
			err = fmt.Errorf("bucketName invalid or not present")
		}
	default:
		err = fmt.Errorf("type: %v not supported", r.Type)
	}
	if err != nil {
		return nil, status.Errorf(
			codes.Code(code.Code_INVALID_ARGUMENT),
			"invalid argument err: %s", err.Error())
	}
	return r, nil
}

func (*warehouseGRPC) ValidateObjectStorageDestination(ctx context.Context, request *proto.ValidateObjectStorageRequest) (response *proto.ValidateObjectStorageResponse, err error) {
	r, err := validateObjectStorageRequestBody(request)
	if err != nil {
		return nil, err
	}

	err = validateObjectStorage(ctx, r)
	if err != nil {

		if errors.As(err, &InvalidDestinationCredErr{}) {
			return &proto.ValidateObjectStorageResponse{
				IsValid: false,
				Error:   err.Error(),
			}, nil
		}

		return &proto.ValidateObjectStorageResponse{},
			status.Errorf(codes.Code(code.Code_INTERNAL), "unable to handle validate storage request call: %s", err)
	}

	return &proto.ValidateObjectStorageResponse{
		IsValid: true,
		Error:   "",
	}, nil
}

func (*warehouseGRPC) CountWHUploadsToRetry(ctx context.Context, req *proto.RetryWHUploadsRequest) (response *proto.RetryWHUploadsResponse, err error) {
	retryReq := &RetryRequest{
		WorkspaceID:     req.WorkspaceId,
		SourceID:        req.SourceId,
		DestinationID:   req.DestinationId,
		DestinationType: req.DestinationType,
		IntervalInHours: req.IntervalInHours,
		ForceRetry:      req.ForceRetry,
		UploadIds:       req.UploadIds,
		API:             UploadAPI,
	}
	r, err := retryReq.UploadsToRetry(ctx)
	response = &proto.RetryWHUploadsResponse{
		Count:      r.Count,
		Message:    r.Message,
		StatusCode: r.StatusCode,
	}
	return
}

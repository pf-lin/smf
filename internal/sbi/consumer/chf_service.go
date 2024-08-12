package consumer

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/free5gc/nas/nasConvert"
	"github.com/free5gc/openapi"
	"github.com/free5gc/openapi/chf/ConvergedCharging"
	"github.com/free5gc/openapi/models"
	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/logger"
)

type nchfService struct {
	consumer *Consumer

	ConvergedChargingMu sync.RWMutex

	ConvergedChargingClients map[string]*ConvergedCharging.APIClient
}

func (s *nchfService) getConvergedChargingClient(uri string) *ConvergedCharging.APIClient {
	if uri == "" {
		return nil
	}
	s.ConvergedChargingMu.RLock()
	client, ok := s.ConvergedChargingClients[uri]
	if ok {
		s.ConvergedChargingMu.RUnlock()
		return client
	}

	configuration := ConvergedCharging.NewConfiguration()
	configuration.SetBasePath(uri)
	client = ConvergedCharging.NewAPIClient(configuration)

	s.ConvergedChargingMu.RUnlock()
	s.ConvergedChargingMu.Lock()
	defer s.ConvergedChargingMu.Unlock()
	s.ConvergedChargingClients[uri] = client
	return client
}

func (s *nchfService) buildConvergedChargingRequest(smContext *smf_context.SMContext,
	multipleUnitUsage []models.ChfConvergedChargingMultipleUnitUsage,
) *models.ChfConvergedChargingChargingDataRequest {
	var triggers []models.ChfConvergedChargingTrigger

	smfContext := s.consumer.Context()
	date := time.Now()

	for _, unitUsage := range multipleUnitUsage {
		for _, usedUnit := range unitUsage.UsedUnitContainer {
			triggers = append(triggers, usedUnit.Triggers...)
		}
	}

	req := &models.ChfConvergedChargingChargingDataRequest{
		ChargingId:           smContext.ChargingID,
		SubscriberIdentifier: smContext.Supi,
		NfConsumerIdentification: &models.ChfConvergedChargingNfIdentification{
			NodeFunctionality: models.ChfConvergedChargingNodeFunctionality_SMF,
			NFName:            smfContext.Name,
			// not sure if NFIPv4Address is RegisterIPv4 or BindingIPv4
			NFIPv4Address: smfContext.RegisterIPv4,
		},
		InvocationTimeStamp: &date,
		Triggers:            triggers,
		PDUSessionChargingInformation: &models.ChfConvergedChargingPduSessionChargingInformation{
			ChargingId: smContext.ChargingID,
			UserInformation: &models.ChfConvergedChargingUserInformation{
				ServedGPSI: smContext.Gpsi,
				ServedPEI:  smContext.Pei,
			},
			PduSessionInformation: &models.ChfConvergedChargingPduSessionInformation{
				PduSessionID: smContext.PDUSessionID,
				NetworkSlicingInfo: &models.NetworkSlicingInfo{
					SNSSAI: smContext.SNssai,
				},

				PduType: nasConvert.PDUSessionTypeToModels(smContext.SelectedPDUSessionType),
				ServingNetworkFunctionID: &models.ChfConvergedChargingServingNetworkFunctionId{
					ServingNetworkFunctionInformation: &models.ChfConvergedChargingNfIdentification{
						NodeFunctionality: models.ChfConvergedChargingNodeFunctionality_AMF,
					},
				},
				DnnId: smContext.Dnn,
			},
		},
		NotifyUri: fmt.Sprintf("%s://%s:%d/nsmf-callback/notify_%s",
			smfContext.URIScheme,
			smfContext.RegisterIPv4,
			smfContext.SBIPort,
			smContext.Ref,
		),
		MultipleUnitUsage: multipleUnitUsage,
	}

	return req
}

func (s *nchfService) SendConvergedChargingRequest(
	smContext *smf_context.SMContext,
	requestType smf_context.RequestType,
	multipleUnitUsage []models.ChfConvergedChargingMultipleUnitUsage,
) (
	*models.ChfConvergedChargingChargingDataResponse, *models.ProblemDetails, error,
) {
	logger.ChargingLog.Info("Handle SendConvergedChargingRequest")

	req := s.buildConvergedChargingRequest(smContext, multipleUnitUsage)

	ctx, pd, err := smf_context.GetSelf().
		GetTokenCtx(models.ServiceName_NCHF_CONVERGEDCHARGING, models.NrfNfManagementNfType_CHF)
	if err != nil {
		return nil, pd, err
	}

	if smContext.SelectedCHFProfile.NfServices == nil {
		errMsg := "no CHF found"
		return nil, openapi.ProblemDetailsDataNotFound(errMsg), fmt.Errorf(errMsg)
	}

	var client *ConvergedCharging.APIClient
	// Create Converged Charging Client for this SM Context
	for _, service := range smContext.SelectedCHFProfile.NfServices {
		if service.ServiceName == models.ServiceName_NCHF_CONVERGEDCHARGING {
			client = s.getConvergedChargingClient(service.ApiPrefix)
		}
	}
	if client == nil {
		errMsg := "no CONVERGEDCHARGING-CHF found"
		return nil, openapi.ProblemDetailsDataNotFound(errMsg), fmt.Errorf(errMsg)
	}

	// select the appropriate converged charging service based on trigger type
	switch requestType {
	case smf_context.CHARGING_INIT:
		postChargingDataRequest := &ConvergedCharging.PostChargingDataRequest{
			ChfConvergedChargingChargingDataRequest: req,
		}
		rspPost, localErr := client.DefaultApi.PostChargingData(ctx, postChargingDataRequest)

		switch err := localErr.(type) {
		case openapi.GenericOpenAPIError:
			errorModel := err.Model().(ConvergedCharging.PostChargingDataError)
			chargingDataRef := strings.Split(errorModel.Location, "/")
			smContext.ChargingDataRef = chargingDataRef[len(chargingDataRef)-1]
			return nil, &errorModel.ProblemDetails, nil
		case error:
			problemDetail := models.ProblemDetails{
				Title:  "Internal Error",
				Status: http.StatusInternalServerError,
				Detail: err.Error(),
			}
			return nil, &problemDetail, nil
		case nil:
			return &rspPost.ChfConvergedChargingChargingDataResponse, nil, nil
		default:
			return nil, nil, openapi.ReportError("server no response")
		}
	case smf_context.CHARGING_UPDATE:
		updateChargingDataRequest := &ConvergedCharging.UpdateChargingDataRequest{
			ChargingDataRef:                         &smContext.ChargingDataRef,
			ChfConvergedChargingChargingDataRequest: req,
		}
		rspUpdate, localErr := client.DefaultApi.UpdateChargingData(ctx, updateChargingDataRequest)

		switch err := localErr.(type) {
		case openapi.GenericOpenAPIError:
			errorModel := err.Model().(ConvergedCharging.UpdateChargingDataError)
			return nil, &errorModel.ProblemDetails, nil
		case error:
			problemDetail := models.ProblemDetails{
				Title:  "Internal Error",
				Status: http.StatusInternalServerError,
				Detail: err.Error(),
			}
			return nil, &problemDetail, nil
		case nil:
			return &rspUpdate.ChfConvergedChargingChargingDataResponse, nil, nil
		default:
			return nil, nil, openapi.ReportError("server no response")
		}
	case smf_context.CHARGING_RELEASE:
		releaseChargingDataRequest := &ConvergedCharging.ReleaseChargingDataRequest{
			ChargingDataRef:                         &smContext.ChargingDataRef,
			ChfConvergedChargingChargingDataRequest: req,
		}
		_, localErr := client.DefaultApi.ReleaseChargingData(ctx, releaseChargingDataRequest)

		switch err := localErr.(type) {
		case openapi.GenericOpenAPIError:
			errorModel := err.Model().(ConvergedCharging.ReleaseChargingDataError)
			return nil, &errorModel.ProblemDetails, nil
		case error:
			problemDetail := models.ProblemDetails{
				Title:  "Internal Error",
				Status: http.StatusInternalServerError,
				Detail: err.Error(),
			}
			return nil, &problemDetail, nil
		case nil:
			return nil, nil, nil
		default:
			return nil, nil, openapi.ReportError("server no response")
		}
	default:
		return nil, nil, openapi.ReportError("invalid request type")
	}
}

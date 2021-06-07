package azuremonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana/pkg/infra/httpclient"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/plugins/backendplugin"
	"github.com/grafana/grafana/pkg/plugins/backendplugin/coreplugin"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/setting"
)

const (
	timeSeries = "time_series"
	dsName     = "grafana-azure-monitor-datasource"
)

var (
	azlog           = log.New("tsdb.azuremonitor")
	legendKeyFormat = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
)

func init() {
	registry.Register(&registry.Descriptor{
		Name:         "AzureMonitorService",
		InitPriority: registry.Low,
		Instance:     &Service{},
	})
}

type Service struct {
	PluginManager        plugins.Manager       `inject:""`
	HTTPClientProvider   httpclient.Provider   `inject:""`
	Cfg                  *setting.Cfg          `inject:""`
	BackendPluginManager backendplugin.Manager `inject:""`
}

type azureMonitorSettings struct {
	AppInsightsAppId             string `json:"appInsightsAppId"`
	AzureLogAnalyticsSameAs      bool   `json:"azureLogAnalyticsSameAs"`
	ClientId                     string `json:"clientId"`
	CloudName                    string `json:"cloudName"`
	LogAnalyticsClientId         string `json:"logAnalyticsClientId"`
	LogAnalyticsDefaultWorkspace string `json:"logAnalyticsDefaultWorkspace"`
	LogAnalyticsSubscriptionId   string `json:"logAnalyticsSubscriptionId"`
	LogAnalyticsTenantId         string `json:"logAnalyticsTenantId"`
	SubscriptionId               string `json:"subscriptionId"`
	TenantId                     string `json:"tenantId"`
	AzureAuthType                string `json:"azureAuthType,omitempty"`
}

type datasourceInfo struct {
	Settings azureMonitorSettings

	HTTPClient              *http.Client
	URL                     string
	JSONData                map[string]interface{}
	DecryptedSecureJSONData map[string]string
	DatasourceID            int64
	OrgID                   int64
}

func NewInstanceSettings(httpClientProvider httpclient.Provider) datasource.InstanceFactoryFunc {
	return func(settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
		opts, err := settings.HTTPClientOptions()
		if err != nil {
			return nil, err
		}

		client, err := httpClientProvider.New(opts)
		if err != nil {
			return nil, err
		}

		jsonData := map[string]interface{}{}
		err = json.Unmarshal(settings.JSONData, &jsonData)
		if err != nil {
			return nil, fmt.Errorf("error reading settings: %w", err)
		}

		azMonitorSettings := azureMonitorSettings{}
		err = json.Unmarshal(settings.JSONData, &azMonitorSettings)
		if err != nil {
			return nil, fmt.Errorf("error reading settings: %w", err)
		}
		model := datasourceInfo{
			Settings:                azMonitorSettings,
			HTTPClient:              client,
			URL:                     settings.URL,
			JSONData:                jsonData,
			DecryptedSecureJSONData: settings.DecryptedSecureJSONData,
			DatasourceID:            settings.ID,
		}

		return model, nil
	}
}

type azDatasourceExecutor interface {
	executeTimeSeriesQuery(ctx context.Context, originalQueries []backend.DataQuery, dsInfo datasourceInfo) (*backend.QueryDataResponse, error)
}

func newExecutor(im instancemgmt.InstanceManager, pm plugins.Manager, httpC httpclient.Provider, cfg *setting.Cfg) *datasource.QueryTypeMux {
	mux := datasource.NewQueryTypeMux()
	executors := map[string]azDatasourceExecutor{
		"Azure Monitor":        &AzureMonitorDatasource{pm, cfg},
		"Application Insights": &ApplicationInsightsDatasource{pm, cfg},
		"Azure Log Analytics":  &AzureLogAnalyticsDatasource{pm, cfg},
		"Insights Analytics":   &InsightsAnalyticsDatasource{pm, cfg},
		"Azure Resource Graph": &AzureResourceGraphDatasource{pm, cfg},
	}
	for dsType := range executors {
		// Make a copy of the string to keep the reference after the iterator
		dst := dsType
		mux.HandleFunc(dsType, func(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
			i, err := im.Get(req.PluginContext)
			if err != nil {
				return nil, err
			}
			dsInfo := i.(datasourceInfo)
			dsInfo.OrgID = req.PluginContext.OrgID
			ds := executors[dst]
			return ds.executeTimeSeriesQuery(ctx, req.Queries, dsInfo)
		})
	}
	return mux
}

func (s *Service) Init() error {
	im := datasource.NewInstanceManager(NewInstanceSettings(s.HTTPClientProvider))
	factory := coreplugin.New(backend.ServeOpts{
		QueryDataHandler: newExecutor(im, s.PluginManager, s.HTTPClientProvider, s.Cfg),
	})

	if err := s.BackendPluginManager.Register(dsName, factory); err != nil {
		azlog.Error("Failed to register plugin", "error", err)
	}
	return nil
}

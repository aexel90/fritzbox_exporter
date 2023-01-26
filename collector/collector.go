package collector

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"regexp"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/aexel90/fritzbox_exporter/lua"
	"github.com/aexel90/fritzbox_exporter/metric"
	"github.com/aexel90/fritzbox_exporter/upnp"
)

// Collector instance
type Collector struct {
	metrics           []*metric.Metric
	labelValueRenames []*metric.LabelRename
	exporter          interface{}
	gateway           string
}

// NewUpnpCollector initialization
func NewUpnpCollector(metricsFile *metric.MetricsFile, URL string, username string, password string, gateway string) (*Collector, error) {

	initDescAndType(metricsFile.Metrics)
	err := initLabelRenames(metricsFile.LabelRenames)
	if err != nil {
		return nil, err
	}

	upnpExporter := upnp.Exporter{
		BaseURL:  URL,
		Username: username,
		Password: password,
	}
	err = upnpExporter.LoadServices()
	if err != nil {
		return nil, err
	}

	return &Collector{metricsFile.Metrics, metricsFile.LabelRenames, &upnpExporter, gateway}, nil
}

// NewLuaCollector initialization
func NewLuaCollector(metricsFile *metric.MetricsFile, URL string, username string, password string, gateway string) (*Collector, error) {

	initDescAndType(metricsFile.Metrics)
	err := initLabelRenames(metricsFile.LabelRenames)
	if err != nil {
		return nil, err
	}

	luaExporter := lua.Exporter{
		BaseURL:  URL,
		Username: username,
		Password: password,
	}

	return &Collector{metricsFile.Metrics, metricsFile.LabelRenames, &luaExporter, gateway}, nil
}

// Describe for prometheus
func (collector *Collector) Describe(ch chan<- *prometheus.Desc) {

	for _, metric := range collector.metrics {
		ch <- metric.Desc
	}
}

// Collect for prometheus
func (collector *Collector) Collect(ch chan<- prometheus.Metric) {

	err := collector.collect()
	if err != nil {
		fmt.Println("Error: ", err)
	}

	collector.addGatewayGeneric()

	err = collector.getResult()
	if err != nil {
		fmt.Println("Error: ", err)
	}

	for _, m := range collector.metrics {
		for _, promResult := range m.PromResult {
			ch <- prometheus.MustNewConstMetric(promResult.PromDesc, promResult.PromValueType, promResult.Value, promResult.LabelValues...)
		}
	}
}

// Test collector metrics
func (collector *Collector) Test(resultFile string) {

	err := collector.collect()
	if err != nil {
		fmt.Println("Error: ", err)
	}

	collector.addGatewayGeneric()

	err = collector.getResult()
	if err != nil {
		fmt.Println("Error: ", err)
	}

	collector.printResult()

	if resultFile != "" {

		jsonString, err := json.MarshalIndent(collector.metrics, "", "\t")
		if err != nil {
			fmt.Println("Error: ", err)
		}

		err = ioutil.WriteFile(resultFile, jsonString, 0644)
		if err != nil {
			fmt.Printf("Failed writing JSON file '%s': %s\n", resultFile, err.Error())
		}
	}
}

func (collector *Collector) printResult() {

	for _, m := range collector.metrics {
		fmt.Printf("Metric: %v\n", m.PromDesc.FqName)
		fmt.Printf(" - Exporter Result: %v\n", m.MetricResult)

		for _, promResult := range m.PromResult {

			fmt.Printf("   - prom desc: %v\n", promResult.PromDesc)
			fmt.Printf("     - prom metric type: %v\n", promResult.PromValueType)
			fmt.Printf("     - prom metric value: %v\n", uint64(promResult.Value))
			fmt.Printf("     - prom label values: %v\n", promResult.LabelValues)
		}
	}
}

func (collector *Collector) collect() error {

	var err error
	switch collector.exporter.(type) {
	case *lua.Exporter:
		luaExporter := collector.exporter.(*lua.Exporter)
		err = luaExporter.Collect(collector.metrics)
	case *upnp.Exporter:
		upnpExporter := collector.exporter.(*upnp.Exporter)
		err = upnpExporter.Collect(collector.metrics)
	}
	if err != nil {
		return err
	}
	return nil
}

func (collector *Collector) getResult() (err error) {

	for _, m := range collector.metrics {
		m.PromResult = nil
		for _, metricResult := range m.MetricResult {

			labelValues, err := getLabelValues(m.PromDesc.VarLabels, metricResult, collector.gateway, collector.labelValueRenames)
			if err != nil {
				return err
			}
			resultValue, err := getResultValue(m.ResultKey, metricResult, m.OkValue)
			if err != nil {
				return err
			}

			result := metric.PrometheusResult{PromDesc: m.Desc, PromValueType: m.Type, Value: resultValue, LabelValues: labelValues}
			m.PromResult = append(m.PromResult, &result)
		}
	}

	return nil
}

func (collector *Collector) addGatewayGeneric() {

	for _, metric := range collector.metrics {
		for _, metricResult := range metric.MetricResult {
			if metricResult != nil {
				metricResult["gateway"] = collector.gateway
			} else {
				fmt.Printf("Error: metricResult is nil for metric %v\n", metric)
			}
		}
	}
}

func initDescAndType(metrics []*metric.Metric) {

	for _, metric := range metrics {

		labels := make([]string, len(metric.PromDesc.VarLabels))
		for i, l := range metric.PromDesc.VarLabels {
			labels[i] = strings.ToLower(l)
		}

		metric.Desc = prometheus.NewDesc(metric.PromDesc.FqName, metric.PromDesc.Help, labels, metric.PromDesc.FixedLabels)
		metric.Type = getValueType(metric.PromType)
	}
}

func initLabelRenames(labelRenames []*metric.LabelRename) error {

	for _, rename := range labelRenames {
		regex, err := regexp.Compile(rename.MatchRegex)
		if err != nil {
			err := fmt.Errorf("Error compiling regex: %v", err)
			return err
		}
		rename.Pattern = *regex
	}
	return nil
}

func getResultValue(key string, result map[string]interface{}, okValue string) (float64, error) {

	if key == "" {
		key = "result"
	}

	value := result[key]
	var floatValue float64

	switch tval := value.(type) {
	case float64:
		floatValue = tval
	case int:
		floatValue = float64(tval)
	case uint64:
		floatValue = float64(tval)
	case bool:
		if tval {
			floatValue = 1
		} else {
			floatValue = 0
		}
	case string:
		if tval == okValue {
			floatValue = 1
		} else {
			floatValue = 0
		}
	default:
		//collect_errors.Inc()
		return 0, fmt.Errorf("[getResultValue] %v in %v - unknown type: %T", key, result, value)
	}
	return floatValue, nil
}

func getLabelValues(labelNames []string, result map[string]interface{}, gateway string, labelRenames []*metric.LabelRename) ([]string, error) {

	labelValues := []string{}
	for _, labelname := range labelNames {

		var labelValue string

		if labelname == "gateway" {
			labelValue = gateway
		} else {
			labelValue = fmt.Sprintf("%v", result[labelname])
		}

		renameLabel(&labelValue, labelRenames)
		labelValue = strings.ToLower(labelValue)
		labelValues = append(labelValues, labelValue)
	}
	return labelValues, nil
}

func getValueType(vt string) (valueType prometheus.ValueType) {

	var valueTypes = map[string]prometheus.ValueType{
		"CounterValue": prometheus.CounterValue,
		"GaugeValue":   prometheus.GaugeValue,
		"UntypedValue": prometheus.UntypedValue,
	}
	valueType = valueTypes[vt]
	if valueType == 0 {
		valueType = prometheus.UntypedValue
	}
	return
}

func renameLabel(labelValue *string, labelRenames []*metric.LabelRename) {

	for _, lblRen := range labelRenames {
		if lblRen.Pattern.MatchString(*labelValue) {
			*labelValue = lblRen.RenameLabel
		}
	}
}

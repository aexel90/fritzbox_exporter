package upnp

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/aexel90/fritzbox_exporter/metric"
	"github.com/prometheus/client_golang/prometheus"
)

const textXML = `text/xml; charset="utf-8"`
const soapActionParamXML = `<%s>%s</%s>`
const soapActionXML = `<?xml version="1.0" encoding="utf-8"?>` +
	`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
	`<s:Body><u:%s xmlns:u=%s>%s</u:%s xmlns:u=%s></s:Body>` +
	`</s:Envelope>`

// Exporter struct
type Exporter struct {
	BaseURL    string
	Username   string
	Password   string
	Device     Device `xml:"device"`
	Services   map[string]*Service
	AuthHeader string
}

// Device struct
type Device struct {
	Services         []*Service `xml:"serviceList>service"`
	SubDevices       []*Device  `xml:"deviceList>device"`
	DeviceType       string     `xml:"deviceType"`
	FriendlyName     string     `xml:"friendlyName"`
	Manufacturer     string     `xml:"manufacturer"`
	ManufacturerURL  string     `xml:"manufacturerURL"`
	ModelDescription string     `xml:"modelDescription"`
	ModelName        string     `xml:"modelName"`
	ModelNumber      string     `xml:"modelNumber"`
	ModelURL         string     `xml:"modelURL"`
	UDN              string     `xml:"UDN"`
	PresentationURL  string     `xml:"presentationURL"`
}

// Service struct
type Service struct {
	ServiceType    string `xml:"serviceType"`
	ServiceID      string `xml:"serviceId"`
	ControlURL     string `xml:"controlURL"`
	EventSubURL    string `xml:"eventSubURL"`
	SCPDUrl        string `xml:"SCPDURL"`
	Actions        map[string]*Action
	StateVariables []*StateVariable
}

// Argument struct
type Argument struct {
	Name                 string `xml:"name"`
	Direction            string `xml:"direction"`
	RelatedStateVariable string `xml:"relatedStateVariable"`
	StateVariable        *StateVariable
}

// StateVariable struct
type StateVariable struct {
	Name         string `xml:"name"`
	DataType     string `xml:"dataType"`
	DefaultValue string `xml:"defaultValue"`
}

// Action struct
type Action struct {
	service     *Service
	Name        string      `xml:"name"`
	Arguments   []*Argument `xml:"argumentList>argument"`
	ArgumentMap map[string]*Argument
}

type scpdRoot struct {
	Actions        []*Action        `xml:"actionList>action"`
	StateVariables []*StateVariable `xml:"serviceStateTable>stateVariable"`
}

// ActionArgument struct
type ActionArgument struct {
	Name  string
	Value interface{}
}

// SoapEnvelope struct
type SoapEnvelope struct {
	XMLName xml.Name `xml:"http://schemas.xmlsoap.org/soap/envelope/ Envelope"`
	Body    SoapBody
}

// SoapBody struct
type SoapBody struct {
	XMLName xml.Name `xml:"http://schemas.xmlsoap.org/soap/envelope/ Body"`
	Fault   SoapFault
}

// SoapFault struct
type SoapFault struct {
	XMLName     xml.Name    `xml:"http://schemas.xmlsoap.org/soap/envelope/ Fault"`
	FaultCode   string      `xml:"faultcode"`
	FaultString string      `xml:"faultstring"`
	Detail      FaultDetail `xml:"detail"`
}

// FaultDetail struct
type FaultDetail struct {
	FaultError FaultError `xml:"UPnPError"`
}

// FaultError strcut
type FaultError struct {
	ErrorCode        int    `xml:"errorCode"`
	ErrorDescription string `xml:"errorDescription"`
}

type collectEntry struct {
	Service   string                 `json:"service"`
	Action    string                 `json:"action"`
	Arguments []*Argument            `json:"arguments"`
	Result    map[string]interface{} `json:"result"`
}

var (
	collectErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fritzbox_exporter_collect_errors",
		Help: "Number of collection errors.",
	})
)

// IsGetOnly Returns if the action seems to be a query for information.
// This is determined by checking if the action has no input arguments and at least one output argument.
func (action *Action) IsGetOnly() bool {
	for _, argument := range action.Arguments {
		if argument.Direction == "in" {
			return false
		}
	}
	return len(action.Arguments) > 0
}

// LoadServices loads the services tree from device
func (exporter *Exporter) LoadServices() error {

	// disable certificate validation, since fritz.box uses self signed cert
	if strings.HasPrefix(exporter.BaseURL, "https://") {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	//igddesc.xml
	err := exporter.load("igddesc.xml")
	if err != nil {
		return err
	}

	// tr64desc.xml
	err = exporter.load("tr64desc.xml")
	if err != nil {
		return err
	}

	// fill services
	exporter.Services = make(map[string]*Service)
	err = exporter.fillServicesForDevice(&exporter.Device)
	if err != nil {
		return err
	}
	return nil
}

// CollectAll available upnp metrics
func CollectAll(URL string, username string, password string, resultFile string) {

	upnpExporter := Exporter{BaseURL: URL, Username: username, Password: password}

	err := upnpExporter.LoadServices()
	if err != nil {
		fmt.Println(err)
		return
	}

	jsonContent := []collectEntry{}

	serviceKeys := []string{}
	for serviceKey := range upnpExporter.Services {
		serviceKeys = append(serviceKeys, serviceKey)
	}
	sort.Strings(serviceKeys)

	for _, serviceKey := range serviceKeys {
		service := upnpExporter.Services[serviceKey]
		fmt.Printf("collecting service '%s' (Url: %s)...\n", serviceKey, service.ControlURL)

		actionKeys := []string{}
		for actionKey := range service.Actions {
			actionKeys = append(actionKeys, actionKey)
		}
		sort.Strings(actionKeys)

		for _, actionKey := range actionKeys {
			action := service.Actions[actionKey]

			var result map[string]interface{}

			if !action.IsGetOnly() {
				result = make(map[string]interface{})
				var errorResult = "... not calling since arguments required or no output"
				result["error"] = errorResult

			} else {
				result, err = upnpExporter.call(action, nil)
				if err != nil {
					result = make(map[string]interface{})
					var errorResult = fmt.Sprintf("FAILED:%s", err.Error())
					result["error"] = errorResult
				}
			}

			jsonContent = append(jsonContent, collectEntry{
				Service:   serviceKey,
				Action:    action.Name,
				Arguments: action.Arguments,
				Result:    result,
			})
		}
	}

	jsonString, err := json.MarshalIndent(jsonContent, "", "\t")
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(string(jsonString))

	if resultFile != "" {
		err = ioutil.WriteFile(resultFile, jsonString, 0644)
		if err != nil {
			fmt.Printf("Failed writing JSON file '%s': %s\n", resultFile, err.Error())
		}
	}
}

// Collect func
func (exporter *Exporter) Collect(metrics []*metric.Metric) error {

	var cachedResults = make(map[string]map[string]interface{})

	for _, metric := range metrics {

		metric.MetricResult = nil // remove already collected metrics

		result, err := exporter.request(cachedResults, metric)
		if err != nil {
			fmt.Println(err.Error())
			collectErrors.Inc()
			exporter.AuthHeader = "" // clear Authheader in case of error, so force reauthentication
			return err
		}
		metric.MetricResult = result
	}
	return nil
}

func (exporter *Exporter) load(path string) error {

	HTTPResponse, err := http.Get(fmt.Sprintf("%s/%s", exporter.BaseURL, path))
	if err != nil {
		return err
	}
	defer HTTPResponse.Body.Close()

	XMLDecoder := xml.NewDecoder(HTTPResponse.Body)
	err = XMLDecoder.Decode(exporter)
	if err != nil {
		return err
	}
	return nil
}

func (exporter *Exporter) fillServicesForDevice(device *Device) error {

	for _, service := range device.Services {

		HTTPResponse, err := http.Get(exporter.BaseURL + service.SCPDUrl)
		if err != nil {
			return err
		}
		defer HTTPResponse.Body.Close()

		var scpd scpdRoot
		dec := xml.NewDecoder(HTTPResponse.Body)
		err = dec.Decode(&scpd)
		if err != nil {
			return err
		}

		service.StateVariables = scpd.StateVariables
		service.Actions = make(map[string]*Action)

		for _, action := range scpd.Actions {
			service.Actions[action.Name] = action
		}

		for _, action := range service.Actions {
			action.service = service
			action.ArgumentMap = make(map[string]*Argument)

			for _, argument := range action.Arguments {
				for _, stateVariable := range service.StateVariables {
					if argument.RelatedStateVariable == stateVariable.Name {
						argument.StateVariable = stateVariable
					}
				}
				action.ArgumentMap[argument.Name] = argument
			}
		}
		exporter.Services[service.ServiceType] = service
	}
	for _, subDevice := range device.SubDevices {
		err := exporter.fillServicesForDevice(subDevice)
		if err != nil {
			return err
		}
	}
	return nil
}

func (exporter *Exporter) request(cachedResults map[string]map[string]interface{}, m *metric.Metric) ([]map[string]interface{}, error) {

	var allResults []map[string]interface{}

	var actArg *ActionArgument
	if m.ActionArgument != nil {
		a := m.ActionArgument
		var value interface{}
		value = a.Value

		if a.ProviderAction != "" {
			providerResult, err := exporter.getActionResult(cachedResults, m.Service, a.ProviderAction, nil)

			if err != nil {
				fmt.Printf("Error getting provider action %s result for %s.%s: %s\n", a.ProviderAction, m.Service, m.Action, err.Error())
				collectErrors.Inc()
			}

			var ok bool
			value, ok = providerResult[a.Value] // Value contains the result name for provider actions
			if !ok {
				fmt.Printf("provider action %s for %s.%s has no result %s", m.Service, m.Action, a.Value, providerResult)
				collectErrors.Inc()
			}
		}

		if a.IsIndex {
			sval := fmt.Sprintf("%v", value)
			count, err := strconv.Atoi(sval)
			if err != nil {
				fmt.Println(err.Error())
				collectErrors.Inc()
			}

			for i := 0; i < count; i++ {
				actArg = &ActionArgument{Name: a.Name, Value: i}
				result, err := exporter.getActionResult(cachedResults, m.Service, m.Action, actArg)

				if err != nil {
					fmt.Println(err.Error())
					collectErrors.Inc()
				}
				allResults = append(allResults, result)
			}
		}
	} else {

		result, err := exporter.getActionResult(cachedResults, m.Service, m.Action, actArg)
		if err != nil {
			fmt.Println(err.Error())
			collectErrors.Inc()
		}
		allResults = append(allResults, result)
	}
	return allResults, nil
}

func (exporter *Exporter) getActionResult(cachedResults map[string]map[string]interface{}, serviceType string, actionName string, actionArg *ActionArgument) (map[string]interface{}, error) {

	key := serviceType + "|" + actionName

	// for calls with argument also add arguement name and value to key
	if actionArg != nil {

		key += "|" + actionArg.Name + "|" + fmt.Sprintf("%v", actionArg.Value)
	}

	cacheEntry := cachedResults[key]
	if cacheEntry == nil {
		service, ok := exporter.Services[serviceType]
		if !ok {
			return nil, fmt.Errorf("service %s not found", serviceType)
		}

		action, ok := service.Actions[actionName]
		if !ok {
			return nil, fmt.Errorf("action %s not found in service %s", actionName, serviceType)
		}

		var err error
		cacheEntry, err = exporter.call(action, actionArg)

		if err != nil {
			return nil, err
		}

		cachedResults[key] = cacheEntry
	}

	return cacheEntry, nil
}

func (exporter *Exporter) call(action *Action, actionArg *ActionArgument) (map[string]interface{}, error) {

	req, err := exporter.createCallHTTPRequest(action, actionArg)
	if err != nil {
		return nil, err
	}

	// reuse prior authHeader, to avoid unnecessary authentication
	if exporter.AuthHeader != "" {
		req.Header.Set("Authorization", exporter.AuthHeader)
	}

	// first try call without auth header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close() // close now, since we make a new request below or fail

		if wwwAuth != "" && exporter.Username != "" && exporter.Password != "" {
			// call failed, but we have a password so calculate header and try again
			err = exporter.getDigestAuthHeader(action, wwwAuth, exporter.Username, exporter.Password)
			if err != nil {
				return nil, fmt.Errorf("%s: %s", action.Name, err.Error())
			}

			req, err = exporter.createCallHTTPRequest(action, actionArg)
			if err != nil {
				return nil, fmt.Errorf("%s: %s", action.Name, err.Error())
			}

			req.Header.Set("Authorization", exporter.AuthHeader)
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				return nil, fmt.Errorf("%s: %s", action.Name, err.Error())
			}
		} else {
			return nil, fmt.Errorf("%s: Unauthorized, but no username and password given", action.Name)
		}
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Sprintf("%s (%d)", http.StatusText(resp.StatusCode), resp.StatusCode)
		if resp.StatusCode == http.StatusInternalServerError {
			buf := new(strings.Builder)
			io.Copy(buf, resp.Body)
			body := buf.String()

			var soapEnv SoapEnvelope
			err := xml.Unmarshal([]byte(body), &soapEnv)
			if err != nil {
				errMsg = fmt.Sprintf("error decoding SOAPFault: %s", err.Error())
			} else {
				soapFault := soapEnv.Body.Fault

				if soapFault.FaultString == "UPnPError" {
					upe := soapFault.Detail.FaultError
					errMsg = fmt.Sprintf("SAOPFault: %s %d (%s)", soapFault.FaultString, upe.ErrorCode, upe.ErrorDescription)
				} else {
					errMsg = fmt.Sprintf("SAOPFault: %s", soapFault.FaultString)
				}
			}
		}
		return nil, fmt.Errorf("%s: %s", action.Name, errMsg)
	}
	return action.parseSoapResponse(resp.Body)
}

func (exporter *Exporter) getDigestAuthHeader(a *Action, wwwAuth string, username string, password string) error {

	if !strings.HasPrefix(wwwAuth, "Digest ") {
		return fmt.Errorf("WWW-Authentication header is not Digest: '%s'", wwwAuth)
	}

	s := wwwAuth[7:]
	d := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		d[strings.Trim(parts[0], "\" ")] = strings.Trim(parts[1], "\" ")
	}

	if d["algorithm"] == "" {
		d["algorithm"] = "MD5"
	} else if d["algorithm"] != "MD5" {
		return fmt.Errorf("digest algorithm not supported: %s != MD5", d["algorithm"])
	}

	if d["qop"] != "auth" {
		return fmt.Errorf("digest qop not supported: %s != auth", d["qop"])
	}

	// calc h1 and h2
	ha1 := fmt.Sprintf("%x", md5.Sum([]byte(username+":"+d["realm"]+":"+password)))
	ha2 := fmt.Sprintf("%x", md5.Sum([]byte("POST:"+a.service.ControlURL)))

	cn := make([]byte, 8)
	rand.Read(cn)
	cnonce := fmt.Sprintf("%x", cn)

	nCounter := 1
	nc := fmt.Sprintf("%08x", nCounter)

	ds := strings.Join([]string{ha1, d["nonce"], nc, cnonce, d["qop"], ha2}, ":")
	response := fmt.Sprintf("%x", md5.Sum([]byte(ds)))

	exporter.AuthHeader = fmt.Sprintf("Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", cnonce=\"%s\", nc=%s, qop=%s, response=\"%s\", algorithm=%s",
		username, d["realm"], d["nonce"], a.service.ControlURL, cnonce, nc, d["qop"], response, d["algorithm"])

	return nil
}

func (exporter *Exporter) createCallHTTPRequest(a *Action, actionArg *ActionArgument) (*http.Request, error) {
	argsString := ""
	if actionArg != nil {
		var buf bytes.Buffer
		sValue := fmt.Sprintf("%v", actionArg.Value)
		xml.EscapeText(&buf, []byte(sValue))
		argsString += fmt.Sprintf(soapActionParamXML, actionArg.Name, buf.String(), actionArg.Name)
	}
	bodystr := fmt.Sprintf(soapActionXML, a.Name, a.service.ServiceType, argsString, a.Name, a.service.ServiceType)

	url := exporter.BaseURL + a.service.ControlURL
	body := strings.NewReader(bodystr)

	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}

	action := fmt.Sprintf("%s#%s", a.service.ServiceType, a.Name)
	req.Header.Set("Content-Type", textXML)
	req.Header.Set("SOAPAction", action)

	return req, nil
}

func (action *Action) parseSoapResponse(reader io.Reader) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	decoder := xml.NewDecoder(reader)

	for {
		t, err := decoder.Token()

		if err == io.EOF {
			return result, nil
		}

		if err != nil {
			return nil, err
		}

		if se, ok := t.(xml.StartElement); ok {
			arg, ok := action.ArgumentMap[se.Name.Local]

			if ok {
				t2, err := decoder.Token()
				if err != nil {
					return nil, err
				}

				var val string
				switch element := t2.(type) {
				case xml.EndElement:
					val = ""
				case xml.CharData:
					val = string(element)
				default:
					return nil, errors.New("invalid SOAP response")
				}

				converted, err := convertResult(val, arg)
				if err != nil {
					return nil, err
				}
				result[arg.StateVariable.Name] = converted
			}
		}
	}
}

func convertResult(val string, arg *Argument) (interface{}, error) {
	switch arg.StateVariable.DataType {

	case "string":
		return val, nil

	case "boolean":
		return bool(val == "1"), nil

	case "ui1", "ui2", "ui4":
		// type ui4 can contain values greater than 2^32!
		res, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return nil, err
		}
		return uint64(res), nil

	case "i4":
		res, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, err
		}
		return int64(res), nil

	case "dateTime", "uuid":
		// data types we don't convert yet
		return val, nil

	default:
		return nil, fmt.Errorf("unknown datatype: %s (%s)", arg.StateVariable.DataType, val)
	}
}

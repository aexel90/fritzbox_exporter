package lua

import (
	"crypto/md5"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/aexel90/fritzbox_exporter/metric"
	"github.com/tidwall/gjson"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

const loginPath = "/login_sid.lua"
const dataPath = "/data.lua"

// Exporter data
type Exporter struct {
	BaseURL  string
	Username string
	Password string
	SID      string
}

type sessionInfo struct {
	XMLName   xml.Name `xml:"SessionInfo"`
	SID       string   `xml:"SID"`
	Challenge string   `xml:"Challenge"`
}

func (exporter *Exporter) extractMetricValuesFromJSON(jsonResult gjson.Result, key string, labelNames []string) (results []map[string]interface{}) {

	if jsonResult.IsArray() == true {
		for _, jsonElement := range jsonResult.Array() {
			if jsonElement.IsArray() {
				return exporter.extractMetricValuesFromJSON(jsonElement, key, labelNames)
			} else if jsonElement.IsObject() {
				result := make(map[string]interface{})

				if key == "" {
					result["result"] = 1
				} else {
					if jsonElement.Get(key).Exists() {
						result[key] = jsonElement.Get(key).Float()
					} else {
						continue
					}
				}
				exporter.getLabelValues(result, labelNames, jsonElement)
				results = append(results, result)
			}
		}
	} else {
		result := make(map[string]interface{})
		if key == "" {
			key = "result"
		}
		result[key] = jsonResult.Float()
		exporter.getLabelValues(result, labelNames, jsonResult)
		results = append(results, result)
	}
	return
}

func (exporter *Exporter) getLabelValues(results map[string]interface{}, labelNames []string, jsonElement gjson.Result) {

	for _, labelName := range labelNames {
		labelValue := jsonElement.Get(labelName).String()
		results[labelName] = labelValue
	}
}

// Collect metrics
func (exporter *Exporter) Collect(metrics []*metric.Metric) (err error) {

	err = exporter.logon()
	if err != nil {
		return err
	}

	for _, m := range metrics {
		// remove already collected metrics
		m.MetricResult = nil

		jsonResponse, err := exporter.request(m.Page)
		if err != nil {
			fmt.Printf("error requesting metric %v: %v\n", m, err)
			return err
		}

		jsonString := string(jsonResponse[:])
		jsonResult := gjson.Get(jsonString, m.ResultPath)
		m.MetricResult = exporter.extractMetricValuesFromJSON(jsonResult, m.ResultKey, m.PromDesc.VarLabels)
	}
	return nil
}

func (exporter *Exporter) logon() error {

	if exporter.SID == "" {
		loginLUA, err := getSessionInfo(exporter.BaseURL + loginPath)
		if err != nil {
			return err
		}

		response := utf16leMd5(loginLUA.Challenge + "-" + exporter.Password)
		responseString := fmt.Sprintf("%x", response)

		sessionInfo, err := getSessionInfo(exporter.BaseURL + loginPath + "?response=" + loginLUA.Challenge + "-" + responseString + "&username=" + exporter.Username)
		if err != nil {
			return err
		}
		exporter.SID = sessionInfo.SID
	}
	return nil
}

func getSessionInfo(gatewayURL string) (*sessionInfo, error) {

	response, err := http.Get(gatewayURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	var session *sessionInfo
	xml.Unmarshal(body, &session)
	return session, nil
}

func utf16leMd5(s string) []byte {

	// https://stackoverflow.com/questions/33710672/golang-encode-string-utf16-little-endian-and-hash-with-md5
	enc := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder()
	hasher := md5.New()
	t := transform.NewWriter(hasher, enc)
	t.Write([]byte(s))
	return hasher.Sum(nil)
}

func (exporter *Exporter) request(page string) ([]byte, error) {

	client := &http.Client{}

	parameters := url.Values{}
	parameters.Add("sid", exporter.SID)
	parameters.Add("page", page)

	request, err := http.NewRequest("POST", exporter.BaseURL+dataPath, strings.NewReader(parameters.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		exporter.SID = ""
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lua request response not OK: %v", response.Status)
	}

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

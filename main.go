package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aws/aws-lambda-go/lambda"
	"go.uber.org/zap"
)

const DEFAULT_TIMEZONE = "Europe/London"
const CONFIG_FILENAME = "config.json"

var ErrRegexMatchFailed error = errors.New("failed to match regex")
var ErrUnexpectedReponse error = errors.New("received unexpected response from server")

var timeRegex, _ = regexp.Compile(`(\d{1,2}):(\d{1,2}) (AM|PM)`)

// var dataRegex, _ = regexp.Compile(`var\sdata\s=\s{\s\s+'.+'\s:\s({[^;]+}),\s+};`)
var dataRegex, _ = regexp.Compile(`var\s+data\s+=\s+{([^;]+),\s+};`)

var logger *zap.Logger
var client *http.Client

type GymDataJSON struct {
	Capacity   int    `json:"capacity"`
	Count      int    `json:"count"`
	LastUpdate string `json:"lastUpdate"`
}

type GymData struct {
	Capacity   int       `json:"capacity"`
	Count      int       `json:"count"`
	LastUpdate time.Time `json:"last_update"`
}

type Header struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Gym struct {
	ShortCode string  `json:"shortcode"`
	Brand     string  `json:"brand"`
	Location  string  `json:"location"`
	Data      GymData `json:"data"`
}

type Endpoint struct {
	Name     string   `json:"name"`
	Brand    string   `json:"brand"`
	URL      string   `json:"url"`
	ID       string   `json:"id"`
	Headers  []Header `json:"headers"`
	Timezone string   `json:"timezone"`
	Gyms     []Gym    `json:"gyms"`
}

func StripWhitespace(str string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			// if the character is a space, drop it
			return -1
		}
		// else keep it in the string
		return r
	}, str)
}

func FetchGymData(endpoint Endpoint) (map[string]GymData, error) {
	location, err := time.LoadLocation(endpoint.Timezone)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/portal/public/%s/occupancy", endpoint.URL, endpoint.ID), nil)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(endpoint.Headers); i++ {
		req.Header.Set(endpoint.Headers[i].Key, endpoint.Headers[i].Value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnexpectedReponse
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var dataJSON map[string]GymDataJSON

	matches := dataRegex.FindStringSubmatch(string(body))
	if len(matches) != 2 {
		return nil, ErrRegexMatchFailed
	}

	fixedJSON := "{" + strings.Replace(matches[1], "'", `"`, -1) + "}"

	if err := json.Unmarshal([]byte(fixedJSON), &dataJSON); err != nil {
		return nil, err
	}

	now := time.Now()

	output := map[string]GymData{}
	for k, v := range dataJSON {
		parsedTime := timeRegex.FindStringSubmatch(v.LastUpdate)
		if err != nil {
			return nil, err
		}
		if len(parsedTime) != 4 {
			return nil, ErrRegexMatchFailed
		}

		tempTime, err := time.ParseInLocation("3:04 PM", parsedTime[0], location)
		if err != nil {
			return nil, err
		}

		output[k] = GymData{
			Capacity:   v.Capacity,
			Count:      v.Count,
			LastUpdate: time.Date(now.Year(), now.Month(), now.Day(), tempTime.Hour(), tempTime.Minute(), 0, 0, time.UTC),
		}
	}

	return output, nil
}

func FetchEndpoint(e Endpoint) (*Endpoint, error) {
	if e.Timezone == "" {
		e.Timezone = DEFAULT_TIMEZONE
	}
	data, err := FetchGymData(e)
	if err != nil {
		logger.Error("get endpoint failed",
			zap.Error(err),
			zap.Reflect("endpoint", e),
		)
		return nil, err
	}
	logger.Debug("got endpoint",
		zap.Reflect("endpoint", e),
		zap.Reflect("data", data),
	)

	gymsMap := map[string]Gym{}
	for i := 0; i < len(e.Gyms); i++ {
		e.Gyms[i].Brand = e.Brand
		gymsMap[e.Gyms[i].ShortCode] = e.Gyms[i]
		if d, ok := data[e.Gyms[i].ShortCode]; ok {
			e.Gyms[i].Data = d
			logger.Info("got gym data",
				zap.Any("gym", e.Gyms[i]),
			)
		}
	}

	return &e, nil
}

func FetchEndpointsFromConfig(c context.Context) error {
	var endpoints []Endpoint

	configRaw := []byte(os.Getenv("CONFIG"))
	if len(configRaw) == 0 {
		logger.Debug("getting config from file", zap.String("path", CONFIG_FILENAME))
		var err error
		configRaw, err = os.ReadFile(CONFIG_FILENAME)
		if err != nil {
			logger.Fatal("could not read config", zap.String("path", CONFIG_FILENAME), zap.Error(err))
			return err
		}
		logger.Debug("got config from file", zap.String("path", CONFIG_FILENAME), zap.String("raw_config", StripWhitespace(string(configRaw))))
	} else {
		logger.Debug("got config from env", zap.String("raw_config", StripWhitespace(string(configRaw))))
	}

	if err := json.Unmarshal(configRaw, &endpoints); err != nil {
		logger.Fatal("could not parse config", zap.Error(err))
		return err
	}

	var wg sync.WaitGroup
	for i := 0; i < len(endpoints); i++ {
		wg.Add(1)
		go func(e Endpoint) {
			defer wg.Done()
			_, _ = FetchEndpoint(e)
		}(endpoints[i])
		wg.Wait()
	}
	return nil
}

func LambdaHandler(c context.Context) {
	FetchEndpointsFromConfig(c)
}

func init() {
	loggerConfigRaw := []byte(os.Getenv("LOGGER_CONFIG"))
	var cfg zap.Config
	if len(loggerConfigRaw) != 0 {
		if err := json.Unmarshal(loggerConfigRaw, &cfg); err != nil {
			panic("could not parse logger config")
		}
		var err error
		logger, err = cfg.Build()
		if err != nil {
			panic("failed to initialise logger")
		}
	} else {
		var err error
		logger, err = zap.NewDevelopment()
		if err != nil {
			panic("failed to initialise logger")
		}
	}

	client = &http.Client{}
}

func LambdaMain() {
	defer logger.Sync()
	lambda.Start(LambdaHandler)
}

func main() {
	defer logger.Sync()
	if os.Getenv("AWS_EXECUTION_ENV") != "" {
		lambda.Start(LambdaHandler)
	} else {
		FetchEndpointsFromConfig()
	}
}

package function

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alexellis/derek/auth"
	"github.com/google/go-github/github"
)

const (
	defaultPrivateKeyName = "private_key.pem"
)

// Handle a build / deploy request - returns empty string for an error
func Handle(req []byte) string {

	c := &http.Client{}

	builderURL := os.Getenv("builder_url")

	event, eventErr := getEvent()
	if eventErr != nil {
		log.Panic(eventErr)
	}

	reader := bytes.NewBuffer(req)
	res, err := http.Post(builderURL+"build", "application/octet-stream", reader)
	if err != nil {
		fmt.Println(err)
		reportStatus("failure", err.Error(), "BUILD", event)
		return ""
	}

	defer res.Body.Close()

	buildStatus, _ := ioutil.ReadAll(res.Body)
	imageName := strings.TrimSpace(string(buildStatus))

	repositoryURL := os.Getenv("repository_url")

	if len(repositoryURL) == 0 {
		fmt.Fprintf(os.Stderr, "repository_url env-var not set")
		os.Exit(1)
	}

	serviceValue := ""

	if len(imageName) > 0 {
		gatewayURL := os.Getenv("gateway_url")

		// Replace image name for "localhost" for deployment
		imageName = repositoryURL + imageName[strings.Index(imageName, ":"):]

		serviceValue = fmt.Sprintf("%s-%s", event.owner, event.service)

		log.Printf("Deploying %s as %s", imageName, serviceValue)

		defaultMemoryLimit := os.Getenv("default_memory_limit")
		if len(defaultMemoryLimit) == 0 {
			defaultMemoryLimit = "20m"
		}

		deploy := deployment{
			Service: serviceValue,
			Image:   imageName,
			Network: "func_functions",
			Labels: map[string]string{
				"Git-Cloud":      "1",
				"Git-Owner":      event.owner,
				"Git-Repo":       event.repository,
				"Git-DeployTime": strconv.FormatInt(time.Now().Unix(), 10), //Unix Epoch string
				"Git-SHA":        event.sha,
			},
			Limits: Limits{
				Memory: defaultMemoryLimit,
			},
		}

		result, err := deployFunction(deploy, gatewayURL, c)

		if err != nil {
			reportStatus("failure", err.Error(), "DEPLOY", event)
			log.Fatal(err.Error())
		}

		log.Println(result)
	}

	reportStatus("success", fmt.Sprintf("function successfully deployed as: %s", serviceValue), "DEPLOY", event)
	return fmt.Sprintf("buildStatus %s %s %s", buildStatus, imageName, res.Status)
}

func getEvent() (*eventInfo, error) {
	var err error
	info := eventInfo{}

	info.service = os.Getenv("Http_Service")
	info.owner = os.Getenv("Http_Owner")
	info.repository = os.Getenv("Http_Repo")
	info.sha = os.Getenv("Http_Sha")
	info.url = os.Getenv("Http_Url")
	info.image = os.Getenv("Http_Image")
	info.installationID, err = strconv.Atoi(os.Getenv("Http_Installation_id"))

	return &info, err
}

func functionExists(deploy deployment, gatewayURL string, c *http.Client) (bool, error) {

	res, err := http.Get(gatewayURL + "system/functions")

	if err != nil {
		fmt.Println(err)
		return false, err
	}

	defer res.Body.Close()

	fmt.Println("functionExists status: " + res.Status)
	result, _ := ioutil.ReadAll(res.Body)

	functions := []function{}
	json.Unmarshal(result, &functions)

	for _, function1 := range functions {
		if function1.Name == deploy.Service {
			return true, nil
		}
	}

	return false, err
}

func deployFunction(deploy deployment, gatewayURL string, c *http.Client) (string, error) {
	exists, err := functionExists(deploy, gatewayURL, c)

	bytesOut, _ := json.Marshal(deploy)
	reader := bytes.NewBuffer(bytesOut)

	fmt.Println("Deploying: " + deploy.Image + " as " + deploy.Service)
	var res *http.Response
	var httpReq *http.Request
	var method string
	if exists {
		method = http.MethodPut
	} else {
		method = http.MethodPost
	}

	httpReq, err = http.NewRequest(method, gatewayURL+"system/functions", reader)
	httpReq.Header.Set("Content-Type", "application/json")

	res, err = c.Do(httpReq)

	if err != nil {
		fmt.Println(err)
		return "", err
	}

	defer res.Body.Close()
	fmt.Println("Deploy status: " + res.Status)
	buildStatus, _ := ioutil.ReadAll(res.Body)

	return string(buildStatus), err
}

func reportStatus(status string, desc string, statusContext string, event *eventInfo) {

	if os.Getenv("report_status") != "true" {
		return
	}

	url := event.url
	if status == "success" {
		publicURL := os.Getenv("gateway_public_url")
		// for success status if gateway's public url id set the deployed
		// function url is used in the commit status
		if publicURL != "" {
			serviceValue := fmt.Sprintf("%s-%s", event.owner, event.service)
			url = publicURL + "function/" + serviceValue
		}
	}

	ctx := context.Background()

	// NOTE: currently vendored derek auth package doesn't take the private key as input;
	// but expect it to be present at : "/run/secrets/derek-private-key"
	// as docker /secrets dir has limited permission we are bound to use secret named
	// as "derek-private-key"
	// the below lines should  be uncommented once the package is updated in derek project
	// privateKeyPath := getPrivateKey()
	// token, tokenErr := auth.MakeAccessTokenForInstallation(os.Getenv("github_app_id"),
	// 	event.installationID, privateKeyPath)

	repoStatus := buildStatus(status, desc, statusContext, url)

	log.Printf("Status: %s, GitHub AppID: %d, Repo: %s, Owner: %s", status, event.installationID, event.repository, event.owner)

	token, tokenErr := auth.MakeAccessTokenForInstallation(os.Getenv("github_app_id"), event.installationID)
	if tokenErr != nil {
		fmt.Printf("failed to report status %v, error: %s\n", repoStatus, tokenErr.Error())
		return
	}

	if token == "" {
		fmt.Printf("failed to report status %v, error: authentication failed Invalid token\n", repoStatus)
		return
	}

	client := auth.MakeClient(ctx, token)

	_, _, apiErr := client.Repositories.CreateStatus(ctx, event.owner, event.repository, event.sha, repoStatus)
	if apiErr != nil {
		fmt.Printf("failed to report status %v, error: %s\n", repoStatus, apiErr.Error())
		return
	}
}

func getPrivateKey() string {
	// we are taking the secrets name from the env, by default it is fixed
	// to private_key.pem.
	// Although user can make the secret with a specific name and provide
	// it in the stack.yaml and also specify the secret name in github.yml
	privateKeyName := os.Getenv("private_key")
	if privateKeyName == "" {
		privateKeyName = defaultPrivateKeyName
	}
	privateKeyPath := "/run/secrets/" + privateKeyName
	return privateKeyPath
}

func buildStatus(status string, desc string, context string, url string) *github.RepoStatus {
	return &github.RepoStatus{State: &status, TargetURL: &url, Description: &desc, Context: &context}
}

type eventInfo struct {
	service        string
	owner          string
	sha            string
	url            string
	repository     string
	installationID int
	image          string
}

type deployment struct {
	Service string
	Image   string
	Network string
	Labels  map[string]string
	Limits  Limits
}

type Limits struct {
	Memory string
}

type function struct {
	Name string
}

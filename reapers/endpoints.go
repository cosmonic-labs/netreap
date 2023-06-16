package reapers

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/client"
	"github.com/google/uuid"
	consul_api "github.com/hashicorp/consul/api"
	nomad_api "github.com/hashicorp/nomad/api"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"
)

const (
	netreapLabelPrefix = "netreap"
	jobIDLabel         = "nomad.job_id"
	namespaceLabel     = "nomad.namespace"
	nomadServicePrefix = "_nomad-task-"
)

// Max number of retries for fetching a newly created service
const maxRetries = 10
const retryWait = time.Second * 2

// Magic number: I took a guess. DO NOT consider this number authoritative
const reapLimit uint = 20

type EndpointReaper struct {
	nomad       *nomad_api.Client
	consul      *consul_api.Client
	cilium      *client.Client
	ctx         context.Context
	cidr        *net.IPNet
	numToReap   uint
	excludeTags []string
}

// NewEndpointReaper creates a new EndpointReaper. This will run an initial reconciliation before
// returning the reaper
func NewEndpointReaper(ctx context.Context, nomad_client *nomad_api.Client, consul_client *consul_api.Client, cidr *net.IPNet, excludeTags []string) (*EndpointReaper, error) {
	// TODO: Improve this once we figure out how we are setting up cilium everywhere
	cilium, err := client.NewDefaultClient()
	if err != nil {
		return nil, fmt.Errorf("error when connecting to cilium agent: %s", err)
	}
	reaper := EndpointReaper{nomad: nomad_client, consul: consul_client, cidr: cidr, cilium: cilium, ctx: ctx, excludeTags: excludeTags}
	// Do the initial reconciliation loop
	if err := reaper.reconcile(); err != nil {
		return nil, fmt.Errorf("unable to perform initial reconciliation: %s", err)
	}
	return &reaper, nil
}

// Run the reaper until the context given in the contructor is cancelled. This function is non
// blocking and will only return errors if something occurs during startup
// return a channel to notify of consul client failures
func (e *EndpointReaper) Run() (<-chan bool, error) {
	// NOTE: Specifying uint max so that it starts from the next available index. If there is a
	// better way to start from latest index, we can change this
	eventChan, err := e.nomad.EventStream().Stream(e.ctx, map[nomad_api.Topic][]string{nomad_api.TopicJob: {}}, math.MaxInt64, &nomad_api.QueryOptions{Namespace: "*"})
	if err != nil {
		return nil, fmt.Errorf("error when starting node event stream: %s", err)
	}
	failChan := make(chan bool, 1)

	go func() {
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()

		for {
			select {
			case <-e.ctx.Done():
				zap.S().Info("Context cancelled, shutting down endpoint reaper")
				return
			case <-tick.C:
				zap.S().Info("Reconciliation loop started")
				if err := e.reconcile(); err != nil {
					zap.S().Errorw("Error occurred during reconcilation, will retry next loop", "error", err)
				}
			case events := <-eventChan:
				if events.Err != nil {
					zap.S().Debugw("Got error message from node event channel", "error", events.Err)
					failChan <- true
					return
				}
				zap.S().Debugf("Got %v job events. Handling...", len(events.Events))
				for _, event := range events.Events {
					switch event.Type {
					case "JobDeregistered":
						if err := e.handleJobDelete(event); err != nil {
							zap.S().Errorw("Error while handling job delete", "error", err)
							continue
						}
					case "JobRegistered":
						go e.handleJobCreate(event)
					default:
						zap.S().Debugf("Ignoring Job event with type of %s", event.Type)
						continue
					}
				}

				// NOTE: Because the process for iterating over all jobs is so intensive (having to
				// pull each job with its own http request), we just keep a counter of how many jobs
				// have been shutdown. Once that counter is hit, we trigger a reconciliation
				if e.numToReap >= reapLimit {
					zap.S().Debug("Reached reap limit, triggering reconciliation")
					// TODO(thomastaylor312): There is a race condition here where that last job
					// that was deleted will not have cleaned up its service from consul yet, so it
					// will appear to still be around. For expediency, I'm just leaving it for now
					// as it will be caught on the next reconciliation. But we might want to find a
					// way to fix it later if we really care
					if err := e.reconcile(); err != nil {
						zap.S().Errorw("Error when performing reconciliation, will retry", "error", err)
					}
				}

			}
		}
	}()

	return failChan, nil
}

type serviceData struct {
	Name string
}

type jobData struct {
	ID        string
	Namespace string
}

func (e *EndpointReaper) includeService(tags []string) bool {
	includeService := true
	for _, tag := range tags {
		for _, t := range e.excludeTags {
			if tag == t {
				includeService = false
				break
			}
		}
	}
	return includeService
}

func (e *EndpointReaper) reconcile() error {
	// Get current endpoints list
	zap.S().Debug("Starting reconciliation")
	catalogsClient := e.consul.Catalog()
	servicesList, _, err := catalogsClient.Services(nil)
	if err != nil {
		return fmt.Errorf("unable to list current consul services")
	}

	servicesToQuery := []serviceData{}
	for k, v := range servicesList {
		if e.includeService(v) {
			servicesToQuery = append(servicesToQuery, serviceData{Name: k})
		}
	}

	zap.S().Debug("Finished fetching service list, constructing set of IP addresses from services", "service_list", servicesToQuery)
	// Create a list of all known IP addresses to job names
	ipMap := map[string]jobData{}
	for _, serviceInfo := range servicesToQuery {
		// Fetch the full job object and skip any one that isn't a cilium one. Yeah, this is gross,
		// but the list endpoint doesn't actually list the full object
		// NOTE: the empty string is an empty "tag" that selects everything
		services, _, err := catalogsClient.Service(serviceInfo.Name, "", nil)
		if err != nil {
			return fmt.Errorf("unable to fetch additional job data for %s: %s", serviceInfo.Name, err)
		}

		for _, service := range services {
			addr := net.ParseIP(service.ServiceAddress)
			if e.cidr.Contains(addr) {
				id := service.ServiceID
				if strings.Contains(id, nomadServicePrefix) {
					svc := strings.Split(id, nomadServicePrefix)
					split := strings.SplitN(svc[1], "-", 6)
					fmt.Println(split)
					allocID := strings.Join(split[0:5], "-")

					if _, err := uuid.Parse(allocID); err != nil {
						return fmt.Errorf("unable to parse alloc id %s: %s", allocID, err)
					}
					ipMap[service.ServiceAddress] = jobData{ID: allocID}
				}
			} else {
				// Skip if this isn't a cilium service
				continue
			}
		}
	}

	zap.S().Debugw("Finished generating current IP list. Fetching endpoints from cilium", "ip_list", ipMap)
	endpoints, err := e.cilium.EndpointList()
	if err != nil {
		return fmt.Errorf("unable to list current cilium endpoints: %s", err)
	}

	var deleteErrors uint = 0

	zap.S().Debug("Checking all endpoints")
	// Loop through all endpoints and check if they are now orphaned. Clean them up if they are.
	// Then, make sure existing endpoints are labeled
	for _, endpoint := range endpoints {
		// NOTE(thomastaylor312): As far as I can tell, our services are all classified as an "init"
		// label. So we should only look at those ones. Other ones are not registered in consul as
		// services necessarily
		if !slices.Contains(endpoint.Status.Labels.SecurityRelevant, "reserved:init") {
			zap.S().Debugw("Endpoint is not an init service, skipping", "labels", endpoint.Status.Labels.SecurityRelevant)
			continue
		}
		zap.S().Debugw("Checking if endpoint still exists", "endpoint_id", endpoint.ID)

		// NOTE(thomastaylor312): In the future if we get more complicated with multiple addresses,
		// this logic will need to be reworked to check if any of them match
		for _, ip := range endpoint.Status.Networking.Addressing {
			zap.S().Debugw("Got ip", "ip", ip)
			data := jobData{}
			datav4, existsv4 := ipMap[ip.IPV4]
			datav6, existsv6 := ipMap[ip.IPV6]
			if !existsv4 && !existsv6 {
				zap.S().Debugw("Endpoint no longer exists, deleting", "endpoint_id", endpoint.ID)
				if err := e.cilium.EndpointDelete(strconv.FormatInt(endpoint.ID, 10)); err != nil {
					deleteErrors++
					zap.S().Errorw("Error when cleaning up IP address. Will retry on next reconciliation", "error", err)
				}
				// If we are deleting, no further logic is needed, so break out of this loop
				break
			}

			// I believe these should be the same job id as they come from the same endpoint.
			if len(datav4.ID) > 0 {
				data = datav4
			} else if len(datav6.ID) > 0 {
				data = datav6
			}

			// Check if the endpoints have labels if it wasn't a delete. Then label if needed
			if !hasLabels(endpoint) && len(data.ID) > 0 {
				zap.S().Debugw("Found an endpoint missing labels. Updating with current job labels", "endpoint_id", endpoint.ID)
				// First get the alloc
				alloc, _, err := e.nomad.Allocations().Info(data.ID, &nomad_api.QueryOptions{Namespace: "*"})
				if err != nil {
					zap.S().Warnw("couldn't fetch alloc from nomad", "alloc_id", data.ID, "error", err)
					return err
				}

				if err := e.labelEndpoint(endpoint.ID, alloc.JobID, alloc.Namespace); err != nil {
					return fmt.Errorf("unable to label job with appropriate metadata: %s", err)
				}
			}
		}
	}

	zap.S().Debugw("Finished reconciliation", "num_errors", deleteErrors)
	e.numToReap = deleteErrors

	return nil
}

func (e *EndpointReaper) handleJobDelete(event nomad_api.Event) error {
	// Filter out only when we get jobs we care about. Each deleted job triggers an event with an
	// evaluation and one with the job spec and `getJobData` handles that for us
	jobID, _, ok := getJobData(event)
	if !ok {
		return nil
	}
	// NOTE: We are just incrementing whenever we see a job delete. There is no way for us to go
	// fetch the associated service from consul once the job is deleted, so we can't check if it was
	// a cilium job
	e.numToReap++
	zap.S().Debugw("Got deleted job. Increasing reaper counter", "count", e.numToReap, "job_id", jobID)
	return nil
}

func (e *EndpointReaper) handleJobCreate(event nomad_api.Event) {
	jobID, namespace, ok := getJobData(event)
	if !ok {
		// We already logged, so just noop out
		return
	}

	job, _, err := e.nomad.Jobs().Info(jobID, &nomad_api.QueryOptions{Namespace: namespace})
	if err != nil {
		zap.S().Warnw("Unable to fetch job info", "error", err, "job_id", jobID)
	}

	var serviceNames []string
	for _, taskGroup := range job.TaskGroups {
		for _, service := range taskGroup.Services {
			// TODO(protochron): Add support for Nomad services
			if service.Provider == "consul" {
				if !e.includeService(service.Tags) {
					zap.S().Debugw("Skipping service because it has an excluded tag", "service_name", service.Name)
					continue
				}
				serviceNames = append(serviceNames, service.Name)
			}
		}
		for _, task := range taskGroup.Tasks {
			for _, service := range task.Services {
				// TODO(protochron): Add support for Nomad services
				if service.Provider == "consul" {
					if !e.includeService(service.Tags) {
						zap.S().Debugw("Skipping service because it has an excluded tag", "service_name", service.Name)
						continue
					}
					serviceNames = append(serviceNames, service.Name)
				}
			}
		}
	}

	for _, serviceName := range serviceNames {
		var services []*consul_api.CatalogService
		for i := 0; i < maxRetries; i++ {
			zap.S().Debugw("Fetching services from consul for job", "job_id", jobID, "retry_num", i+1)
			services, _, err = e.consul.Catalog().Service(serviceName, "", nil)
			if err != nil {
				zap.S().Errorw("Unable to fetch current list of services", "error", err)
				return
			}
			if len(services) == 0 || services == nil {
				zap.S().Debugw("Did not find a ready service in consul", "job_id", jobID, "retry_num", i+1)
				time.Sleep(retryWait)
			} else {
				break
			}
		}
		if len(services) == 0 || services == nil {
			zap.S().Errorw("couldn't find any services associated with the job after multiple retries. Aborting labeling attempt", "job_id", jobID)
			return
		}
		zap.S().Debug("Found services for new job", "job_id", jobID)
		for _, service := range services {
			stringAddr := service.ServiceAddress
			addr := net.ParseIP(stringAddr)
			if !e.cidr.Contains(addr) {
				// Skip if this isn't a cilium service
				zap.S().Debugw("New job is not a cilium service. Skipping further steps", "job_id", jobID)
				return
			}

			// TODO(thomastaylor312): We might want to see if there is a more efficient way than querying
			// all endpoints and looping
			zap.S().Debugw("Finding related cilium endpoint for job", "job_id", jobID)
			endpoints, err := e.cilium.EndpointList()
			if err != nil {
				zap.S().Errorw("Unable to fetch current list of cilium endpoints", "error", err)
				return
			}

			foundEndpoint := false
			for _, endpoint := range endpoints {
				for _, ip := range endpoint.Status.Networking.Addressing {
					if stringAddr == ip.IPV4 || stringAddr == ip.IPV6 {
						foundEndpoint = true
						// If it hasn't already been labeled, label it
						if !hasLabels(endpoint) {
							if err := e.labelEndpoint(endpoint.ID, jobID, namespace); err != nil {
								zap.S().Errorw("Error when labeling endpoint. Will retry on next reconcile", "job_id", jobID, "error", err)
							}
							return
						}
					}
				}
			}
			if !foundEndpoint {
				zap.S().Debugw("Got a cilium job, but was unable to find an endpoint on this node", "job_id", jobID)
			}
		}
	}
}

func (e *EndpointReaper) labelEndpoint(endpointID int64, jobID string, namespace string) error {
	// Fetch the current job
	job, _, err := e.nomad.Jobs().Info(jobID, &nomad_api.QueryOptions{Namespace: namespace})
	if err != nil {
		// If for some reason the job doesn't exist then we don't need to label or fail. But we
		// should warn in the logs
		zap.S().Warnw("couldn't fetch job from nomad", "job_id", jobID, "error", err)
		return err
	}

	// Always add the job id for reference
	labels := models.Labels{fmt.Sprintf("%s:%s=%s", netreapLabelPrefix, jobIDLabel, jobID)}
	labels = append(labels, fmt.Sprintf("%s:%s=%s", netreapLabelPrefix, namespaceLabel, namespace))
	if job.Meta != nil {
		for k, v := range job.Meta {
			labels = append(labels, fmt.Sprintf("nomad:%s=%s", k, v))
		}
	}
	return e.cilium.EndpointLabelsPatch(strconv.FormatInt(endpointID, 10), labels, nil)
}

// Returns the job ID and namespace from an event as well as bool indicating whether or not we were
// able to parse the data
func getJobData(event nomad_api.Event) (string, string, bool) {
	job, err := event.Job()
	if err != nil {
		zap.S().Debugw("Unable to deserialize Job", "event_type", event.Type)
		return "", "", false
	}

	if job == nil {
		zap.S().Debugw("Job was empty", "event_type", event.Type)
		return "", "", false
	}

	return *job.ID, *job.Namespace, true
}

func hasLabels(endpoint *models.Endpoint) bool {
	if endpoint == nil {
		return false
	}
	// If there are any netreaper labels, assume we are ok
	for _, label := range endpoint.Status.Labels.Realized.User {
		if strings.HasPrefix(label, netreapLabelPrefix) {
			return true
		}
	}
	return false
}

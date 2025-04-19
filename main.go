package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// TeamsMessage represents a Microsoft Teams webhook message
type TeamsMessage struct {
	Type       string    `json:"@type"`
	Context    string    `json:"@context"`
	ThemeColor string    `json:"themeColor"`
	Summary    string    `json:"summary"`
	Sections   []Section `json:"sections"`
}

// Section represents a section in a Teams message
type Section struct {
	ActivityTitle    string `json:"activityTitle"`
	ActivitySubtitle string `json:"activitySubtitle"`
	ActivityImage    string `json:"activityImage,omitempty"`
	Facts            []Fact `json:"facts"`
	Text             string `json:"text"`
}

// Fact represents a key-value pair in a Teams message section
type Fact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NotificationQueue manages the rate-limited sending of notifications
type NotificationQueue struct {
	webhookURL string
	ticker     *time.Ticker
	queue      chan *NotificationItem
	wg         sync.WaitGroup
}

// NotificationItem represents a notification to be sent
type NotificationItem struct {
	EventType    string
	ResourceType string
	Object       *unstructured.Unstructured
}

// NewNotificationQueue creates a new notification queue with rate limiting
func NewNotificationQueue(webhookURL string, rateLimit time.Duration) *NotificationQueue {
	q := &NotificationQueue{
		webhookURL: webhookURL,
		ticker:     time.NewTicker(rateLimit),
		queue:      make(chan *NotificationItem, 100), // Buffer up to 100 notifications
	}

	// Start the worker
	q.wg.Add(1)
	go q.processQueue()

	return q
}

// processQueue processes the notification queue at the rate limit
func (q *NotificationQueue) processQueue() {
	defer q.wg.Done()

	for {
		select {
		case item, ok := <-q.queue:
			if !ok {
				// Channel closed, exit
				return
			}
			// Wait for ticker before processing
			<-q.ticker.C
			sendTeamsNotification(q.webhookURL, item.EventType, item.ResourceType, item.Object)
		}
	}
}

// Enqueue adds a notification to the queue
func (q *NotificationQueue) Enqueue(eventType, resourceType string, obj *unstructured.Unstructured) {
	q.queue <- &NotificationItem{
		EventType:    eventType,
		ResourceType: resourceType,
		Object:       obj,
	}
}

// Close stops the notification queue
func (q *NotificationQueue) Close() {
	close(q.queue)
	q.ticker.Stop()
	q.wg.Wait()
}

func main() {
	// Get namespace to watch from environment variable or use default
	namespace := os.Getenv("WATCH_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	// Get Teams webhook URL from environment variable
	teamsWebhookURL := os.Getenv("TEAMS_WEBHOOK_URL")
	if teamsWebhookURL == "" {
		log.Fatal("TEAMS_WEBHOOK_URL environment variable is required")
	}

	// Set up Kubernetes client using in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Error creating in-cluster config: %v", err)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating dynamic client: %v", err)
	}

	// Create factory for dynamic informers
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, time.Minute, namespace, nil)

	// Create notification queue with rate limiting (1 request per 2 seconds)
	notificationQueue := NewNotificationQueue(teamsWebhookURL, 2*time.Second)

	// List of resource types to watch - limited to pods and deployments only
	gvrList := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "pods"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	}

	// Create context for informers
	ctx := context.Background()

	// Set up informers for each resource type with unique handlers to avoid warnings
	for _, gvr := range gvrList {
		informer := factory.ForResource(gvr)
		resourceType := gvr.Resource // Capture the resource type in this scope

		// Add event handlers specific to this resource type
		informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				u := obj.(*unstructured.Unstructured)
				log.Printf("ADDED: %s/%s in namespace %s", resourceType, u.GetName(), u.GetNamespace())
				notificationQueue.Enqueue("ADDED", resourceType, u)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldU := oldObj.(*unstructured.Unstructured)
				newU := newObj.(*unstructured.Unstructured)

				// Only send notification if something important changed
				if oldU.GetResourceVersion() != newU.GetResourceVersion() {
					log.Printf("MODIFIED: %s/%s in namespace %s", resourceType, newU.GetName(), newU.GetNamespace())
					notificationQueue.Enqueue("MODIFIED", resourceType, newU)
				}
			},
			DeleteFunc: func(obj interface{}) {
				u := obj.(*unstructured.Unstructured)
				log.Printf("DELETED: %s/%s in namespace %s", resourceType, u.GetName(), u.GetNamespace())
				notificationQueue.Enqueue("DELETED", resourceType, u)
			},
		})

		// Start informer in its own goroutine
		go informer.Informer().Run(ctx.Done())
		log.Printf("Started informer for %s", resourceType)
	}

	// Start the factory
	factory.Start(ctx.Done())
	log.Printf("Watching for changes in namespace %s", namespace)

	// Set up signal handling for graceful shutdown
	// Note: This requires signal package imports
	// signals := make(chan os.Signal, 1)
	// signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	// <-signals
	// notificationQueue.Close()

	// Keep the application running
	select {}
}

// sendTeamsNotification sends a notification to Microsoft Teams
func sendTeamsNotification(webhookURL, eventType, resourceType string, obj *unstructured.Unstructured) {
	name := obj.GetName()
	namespace := obj.GetNamespace()
	resourceVersion := obj.GetResourceVersion()
	creationTimestamp := obj.GetCreationTimestamp()

	// Get additional details based on resource type
	additionalFacts := []Fact{}

	if resourceType == "pods" {
		// Extract pod-specific details if available
		status, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
		if found {
			additionalFacts = append(additionalFacts, Fact{
				Name:  "Phase",
				Value: status,
			})
		}

		// Extract container details if available
		containers, found, _ := unstructured.NestedSlice(obj.Object, "spec", "containers")
		if found {
			containerNames := []string{}
			for _, c := range containers {
				container, ok := c.(map[string]interface{})
				if ok {
					containerName, found := container["name"].(string)
					if found {
						containerNames = append(containerNames, containerName)
					}
				}
			}
			if len(containerNames) > 0 {
				additionalFacts = append(additionalFacts, Fact{
					Name:  "Containers",
					Value: strings.Join(containerNames, ", "),
				})
			}
		}
	} else if resourceType == "deployments" {
		// Extract deployment-specific details if available
		replicas, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		if found {
			additionalFacts = append(additionalFacts, Fact{
				Name:  "Replicas",
				Value: fmt.Sprintf("%d", replicas),
			})
		}
	}

	// Prepare basic facts
	facts := []Fact{
		{Name: "Resource Type", Value: resourceType},
		{Name: "Resource Name", Value: name},
		{Name: "Event Type", Value: eventType},
		{Name: "Namespace", Value: namespace},
		{Name: "Resource Version", Value: resourceVersion},
		{Name: "Creation Time", Value: creationTimestamp.String()},
	}

	// Add resource-specific facts
	facts = append(facts, additionalFacts...)

	// Prepare message for Teams
	message := TeamsMessage{
		Type:       "MessageCard",
		Context:    "http://schema.org/extensions",
		ThemeColor: getColorForEventType(eventType),
		Summary:    fmt.Sprintf("Kubernetes %s %s in namespace %s", resourceType, eventType, namespace),
		Sections: []Section{
			{
				ActivityTitle:    fmt.Sprintf("Kubernetes %s %s", resourceType, eventType),
				ActivitySubtitle: fmt.Sprintf("Namespace: %s", namespace),
				ActivityImage:    "https://kubernetes.io/images/favicon.png",
				Facts:            facts,
				Text: fmt.Sprintf("A %s resource named '%s' was %s in namespace '%s'.",
					resourceType, name, strings.ToLower(eventType), namespace),
			},
		},
	}

	// Convert message to JSON
	jsonMessage, err := json.Marshal(message)
	if err != nil {
		log.Printf("Error marshaling Teams message: %v", err)
		return
	}

	// Send HTTP POST request to Teams webhook
	resp, err := http.Post(webhookURL, "application/json", strings.NewReader(string(jsonMessage)))
	if err != nil {
		log.Printf("Error sending Teams notification: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("Teams webhook returned non-success status code: %d", resp.StatusCode)

		// Add detailed error logging for specific status codes
		if resp.StatusCode == 429 {
			log.Printf("RATE LIMIT EXCEEDED: Teams webhook is being rate limited. Consider increasing the delay between requests.")
		}
	} else {
		log.Printf("Successfully sent notification for %s '%s' (%s)", resourceType, name, eventType)
	}
}

// getColorForEventType returns a hex color code based on event type
func getColorForEventType(eventType string) string {
	switch eventType {
	case "ADDED":
		return "00FF00" // Green
	case "MODIFIED":
		return "FFFF00" // Yellow
	case "DELETED":
		return "FF0000" // Red
	default:
		return "0078D7" // Default blue
	}
}

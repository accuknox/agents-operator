package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

type AgentConfig struct {
	Agent []struct {
		Name      string `yaml:"name"`
		Container []struct {
			Resource []struct {
				Type    string `yaml:"type"`
				Request []struct {
					Multiplier int `yaml:"multiplier"`
					UpperBound int `yaml:"upper-bound"`
				} `yaml:"request"`
				Limit []struct {
					Multiplier int `yaml:"multiplier"`
					UpperBound int `yaml:"upper-bound"`
				} `yaml:"limit"`
			} `yaml:"resource"`
		} `yaml:"container"`
	} `yaml:"agent"`
}

func numberOfNodes(clientset *kubernetes.Clientset) int {
	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Failed to list nodes: %v", err)
	}

	nodesCount := len(nodes.Items)
	return nodesCount
}

func updateAgentResource(clientset *kubernetes.Clientset, configMap *v1.ConfigMap, index, nodesCount int, agentName, namespace string) error {
	var conf AgentConfig
	err := yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
	if err != nil {
		// handle error
		fmt.Println(err)
	}

	// access the cpu field values
	var cpuIndex int
	for i, resource := range conf.Agent[1].Container[0].Resource {
		if resource.Type == "cpu" {
			cpuIndex = i
			break
		}
	}

	cpuReqMultiplier := conf.Agent[index].Container[0].Resource[cpuIndex].Request[0].Multiplier
	cpuLimitMultiplier := conf.Agent[index].Container[0].Resource[cpuIndex].Limit[0].Multiplier
	cpuLimitUB := conf.Agent[index].Container[0].Resource[cpuIndex].Limit[1].UpperBound

	// access memory field values
	var memIndex int
	for i, resource := range conf.Agent[1].Container[0].Resource {
		if resource.Type == "memory" {
			memIndex = i
			break
		}
	}
	memReqMultiplier := conf.Agent[index].Container[0].Resource[memIndex].Request[0].Multiplier
	memLimitMultiplier := conf.Agent[index].Container[0].Resource[memIndex].Limit[0].Multiplier
	memLimitUB := conf.Agent[index].Container[0].Resource[memIndex].Limit[1].UpperBound

	cpuReq := int64(nodesCount * cpuReqMultiplier)
	cpuLimit := int64(nodesCount * cpuLimitMultiplier)
	if cpuLimit > int64(cpuLimitUB) {
		cpuLimit = int64(cpuLimitUB)
	}

	mebibyte := 1048576
	memReq := int64(nodesCount * memReqMultiplier * mebibyte) // value in Mi
	memLimit := int64(nodesCount * memLimitMultiplier * mebibyte)
	if memReq > int64(memLimitUB*mebibyte) {
		memReq = int64(memLimitUB * mebibyte)
	}
	if memLimit > int64(memLimitUB*mebibyte) {
		memLimit = int64(memLimitUB * mebibyte)
	}

	// Deployment
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), agentName, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("deployment not found %v %v\n", deployment, err)
		return nil
	}

	deploy1, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), agentName, metav1.GetOptions{})
	if err != nil {
		fmt.Printf("deployment not found %v %v\n", deployment, err)
		return nil
	}

	deployment.Spec.Template.Spec.Containers[0].Resources.Requests = v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(cpuReq, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(memReq, resource.BinarySI),
	}

	deployment.Spec.Template.Spec.Containers[0].Resources.Limits = v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(cpuLimit, resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(memLimit, resource.BinarySI),
	}

	// Get the original deployment as raw bytes.
	original, err := json.Marshal(deploy1)
	if err != nil {
		panic(err)
	}

	// Get the modified deployment as raw bytes.
	modified, err := json.Marshal(deployment)
	if err != nil {
		panic(err)
	}

	// Create the patch.
	patch, err := strategicpatch.CreateTwoWayMergePatch(original, modified, appsv1.Deployment{})
	if err != nil {
		panic(err)
	}

	// _, err = clientset.CoreV1().Pods("accuknox-agents").Patch(context.Background(), "shared-informer-agent-79664747c8-28q6h", types.MergePatchType, payloadBytes, metav1.PatchOptions{})
	_, err = clientset.AppsV1().Deployments(namespace).Patch(context.Background(), agentName, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		log.Fatalf("Error patching pod: %v", err)
	}

	return nil
}

func watchConfigMap(clientset *kubernetes.Clientset, namespace, agentConfig string, nodesCount int) {
	// Create a new context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	configMapName := "metadata.name=" + agentConfig
	// Watch for changes to the configmap
	watcher, err := clientset.CoreV1().ConfigMaps(namespace).Watch(ctx, metav1.ListOptions{FieldSelector: configMapName})
	if err != nil {
		panic(err)
	}
	defer watcher.Stop()

	// Loop indefinitely
	for {
		select {
		case <-watcher.ResultChan():
			configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), agentConfig, metav1.GetOptions{})
			if err != nil {
				fmt.Println(err)
				return
			}

			var conf AgentConfig
			err = yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
			if err != nil {
				// handle error
				fmt.Println(err)
			}

			for i, resource := range conf.Agent {
				err = updateAgentResource(clientset, configMap, i, nodesCount, resource.Name, namespace)
				if err != nil {
					panic(err)
				}
			}

			// fmt.Println(configMap.Data)
		case <-time.After(time.Minute):
		}
	}
}

func main() {

	// var memLimit, cpuLimit int

	// get the local kube config
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	config, err := kubeconfig.ClientConfig()
	if err != nil {
		panic(err)
	}

	// create the clientset
	clientset := kubernetes.NewForConfigOrDie(config)

	nodesCount := numberOfNodes(clientset)

	namespace := "accuknox-agents"
	var name string
	agentConfig := "agents-operator-config"

	// Watcher to look for ConfigMap changes
	go watchConfigMap(clientset, namespace, agentConfig, nodesCount)

	// Get the ConfigMap
	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), agentConfig, metav1.GetOptions{})
	if err != nil {
		fmt.Println(err)
		return
	}

	var conf AgentConfig
	err = yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
	if err != nil {
		// handle error
		fmt.Println(err)
	}

	// -----------------------------------------------------------------
	// Create a shared informer factory.
	factory := informers.NewSharedInformerFactory(clientset, time.Second*5)

	dfactory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Second*30, informers.WithNamespace(namespace))
	// -----------------
	// Retrieve the deployment informer.
	deploymentInformer := dfactory.Apps().V1().Deployments().Informer()

	// Set up an event handler for when deployments are added or deleted.
	_, err = deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// Print the name of the newly added deployment
			deployment, ok := obj.(*appsv1.Deployment)
			if ok {
				fmt.Printf("New deployment added: %s\n", deployment.Name)
				//-----------------

				// Loop until all deployments are ready
				for {
					deployments, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
					if err != nil {
						fmt.Printf("deployment not found %v %v\n", deployment, err)
						return
					}

					allReady := true
					for _, deployment := range deployments.Items {
						if deployment.Status.ReadyReplicas != *deployment.Spec.Replicas {
							allReady = false
							break
						}
					}

					if allReady {
						break
					}

					time.Sleep(2 * time.Second)
				}

				//------------------
				for i, resource := range conf.Agent {
					name = resource.Name
					err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
					if err != nil {
						panic(err)
					}
				}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDeployment, ok := oldObj.(*appsv1.Deployment)
			if !ok {
				return
			}
			newDeployment, ok := newObj.(*appsv1.Deployment)
			if !ok {
				return
			}

			// Check if the deployment has been updated and if it is now ready
			if oldDeployment.ResourceVersion != newDeployment.ResourceVersion && newDeployment.Status.ReadyReplicas == *newDeployment.Spec.Replicas {
				fmt.Printf("Deployment updated: %s\n", newDeployment.Name)
				//-----------------

				// Loop until all deployments are ready
				for {
					deployments, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
					if err != nil {
						fmt.Printf("deployment not found %v %v\n", newDeployment, err)
						return
					}

					allReady := true
					for _, deployment := range deployments.Items {
						if deployment.Status.ReadyReplicas != *deployment.Spec.Replicas {
							allReady = false
							break
						}
					}

					if allReady {
						break
					}

					time.Sleep(2 * time.Second)
				}

				//------------------
				for i, resource := range conf.Agent {
					name = resource.Name
					err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
					if err != nil {
						panic(err)
					}
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			// Print the name of the deleted deployment
			deployment, ok := obj.(*appsv1.Deployment)
			if ok {
				fmt.Printf("Deployment deleted: %s\n", deployment.Name)
			}
		},
	})

	// Start the informer
	stopCh := make(chan struct{})
	defer close(stopCh)

	// Run the informer with the stop channel.
	deploymentInformer.Run(stopCh)

	// Retrieve the node informer.
	nodeInformer := factory.Core().V1().Nodes().Informer()

	// Set up an event handler for when nodes are added or deleted.
	_, err = nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// printNodeCount(nodeInformer.GetStore())
			for i, resource := range conf.Agent {
				name = resource.Name
				err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
				if err != nil {
					panic(err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			// printNodeCount(nodeInformer.GetStore())
			for i, resource := range conf.Agent {
				name = resource.Name
				err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
				if err != nil {
					panic(err)
				}
			}
		},
	})

	// Run the informer with the stop channel.
	nodeInformer.Run(stopCh)
}

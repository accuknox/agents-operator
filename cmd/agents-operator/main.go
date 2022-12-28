package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/antonmedv/expr"
	"github.com/rs/zerolog/log"
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
					Value      string `yaml:"value"`
					UpperBound int    `yaml:"upper-bound"`
				} `yaml:"request"`
				Limit []struct {
					Value      string `yaml:"value"`
					UpperBound int    `yaml:"upper-bound"`
				} `yaml:"limit"`
			} `yaml:"resource"`
		} `yaml:"container"`
	} `yaml:"agent"`
}

func numberOfNodes(clientset *kubernetes.Clientset) int {
	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Error().Msgf("Failed to list nodes: %v", err)
	}

	nodesCount := len(nodes.Items)
	return nodesCount
}

func exprEval(valueExpr string, nodesCount int) int {

	env := map[string]interface{}{
		"n": nodesCount,
	}

	compiledExpr, err := expr.Compile(valueExpr, expr.Env(env))
	if err != nil {
		log.Error().Msg(err.Error())
	}

	value, err := expr.Run(compiledExpr, env)
	if err != nil {
		log.Error().Msg(err.Error())
	}

	valueInt, ok := value.(int)
	if !ok {
		log.Error().Msg(err.Error())
	}
	return valueInt
}

func updateAgentResource(clientset *kubernetes.Clientset, configMap *v1.ConfigMap, index, nodesCount int, agentName, namespace string) error {
	var conf AgentConfig
	err := yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
	if err != nil {
		log.Error().Msg(err.Error())
	}

	// access the cpu field values
	var cpuIndex int
	for i, resource := range conf.Agent[1].Container[0].Resource {
		if resource.Type == "cpu" {
			cpuIndex = i
			break
		}
	}

	cpuLimitUB := conf.Agent[index].Container[0].Resource[cpuIndex].Limit[1].UpperBound
	cpuReqValueExpr := conf.Agent[index].Container[0].Resource[cpuIndex].Request[0].Value
	cpuLimitValueExpr := conf.Agent[index].Container[0].Resource[cpuIndex].Limit[0].Value

	// access memory field values
	var memIndex int
	for i, resource := range conf.Agent[1].Container[0].Resource {
		if resource.Type == "memory" {
			memIndex = i
			break
		}
	}

	memLimitUB := conf.Agent[index].Container[0].Resource[memIndex].Limit[1].UpperBound
	memReqValueExpr := conf.Agent[index].Container[0].Resource[memIndex].Request[0].Value
	memLimitValueExpr := conf.Agent[index].Container[0].Resource[memIndex].Limit[0].Value

	cpuReq := int64(exprEval(cpuReqValueExpr, nodesCount))
	cpuLimit := int64(exprEval(cpuLimitValueExpr, nodesCount))

	if cpuLimit > int64(cpuLimitUB) {
		cpuLimit = int64(cpuLimitUB)
	}

	mebibyte := 1048576
	memReq := int64(exprEval(memReqValueExpr, nodesCount) * mebibyte) // value in Mi
	memLimit := int64(exprEval(memLimitValueExpr, nodesCount) * mebibyte)

	if memReq > int64(memLimitUB*mebibyte) {
		memReq = int64(memLimitUB * mebibyte)
	}
	if memLimit > int64(memLimitUB*mebibyte) {
		memLimit = int64(memLimitUB * mebibyte)
	}

	deployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), agentName, metav1.GetOptions{})
	if err != nil {
		log.Error().Msg(err.Error())
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

	originalDeployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), agentName, metav1.GetOptions{})
	if err != nil {
		log.Error().Msg(err.Error())
		return nil
	}

	// Get the original deployment as raw bytes
	original, err := json.Marshal(originalDeployment)
	if err != nil {
		return err
	}

	// Get the modified deployment as raw bytes
	modified, err := json.Marshal(deployment)
	if err != nil {
		return err
	}

	// Create the patch
	patch, err := strategicpatch.CreateTwoWayMergePatch(original, modified, appsv1.Deployment{})
	if err != nil {
		return err
	}

	_, err = clientset.AppsV1().Deployments(namespace).Patch(context.Background(), agentName, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		log.Error().Msgf("Error patching pod: %v", err)
	}

	return nil
}

// watch "agents-operator-config" configMap for changes
func watchConfigMap(clientset *kubernetes.Clientset, namespace, agentConfig string, nodesCount int) {
	// Create a new context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	configMapName := "metadata.name=" + agentConfig
	// Watch for changes to the configmap
	watcher, err := clientset.CoreV1().ConfigMaps(namespace).Watch(ctx, metav1.ListOptions{FieldSelector: configMapName})
	if err != nil {
		log.Error().Msg(err.Error())
		return
	}
	defer watcher.Stop()

	// Loop indefinitely
	for {
		select {
		case <-watcher.ResultChan():
			configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), agentConfig, metav1.GetOptions{})
			if err != nil {
				log.Error().Msg(err.Error())
				return
			}

			var conf AgentConfig
			err = yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
			if err != nil {
				log.Error().Msg(err.Error())
			}

			for i, resource := range conf.Agent {
				err = updateAgentResource(clientset, configMap, i, nodesCount, resource.Name, namespace)
				if err != nil {
					log.Error().Msg(err.Error())
					return
				}
			}

		case <-time.After(time.Minute):
			log.Info().Msgf("Watching the agents-operator configMap changes")
		}
	}
}

// wait for the deployment to be ready
func deploymentReady(clientset *kubernetes.Clientset, namespace string) {
	// Loop until all deployments are ready
	for {
		deployments, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Error().Msgf("Deployment not found: %v", err)
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
}

func main() {
	// get the local kube config
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	config, err := kubeconfig.ClientConfig()
	if err != nil {
		log.Error().Msg(err.Error())
		return
	}

	// create the clientset
	clientset := kubernetes.NewForConfigOrDie(config)

	nodesCount := numberOfNodes(clientset)

	namespace := "accuknox-agents"
	agentConfig := "agents-operator-config"
	var name string

	// Watcher to look for ConfigMap changes
	go watchConfigMap(clientset, namespace, agentConfig, nodesCount)

	// Get the ConfigMap
	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), agentConfig, metav1.GetOptions{})
	if err != nil {
		log.Error().Msg(err.Error())
		return
	}

	var conf AgentConfig
	err = yaml.Unmarshal([]byte(configMap.Data["conf.yaml"]), &conf)
	if err != nil {
		log.Error().Msg(err.Error())
	}

	// Create shared informer factory
	factory := informers.NewSharedInformerFactory(clientset, time.Second*5)

	dfactory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Second*30, informers.WithNamespace(namespace))

	// Retrieve the deployment informer
	deploymentInformer := dfactory.Apps().V1().Deployments().Informer()

	// Set up an event handler for when deployments are added or deleted
	_, err = deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// Print the name of the newly added deployment
			deployment, ok := obj.(*appsv1.Deployment)
			if ok {
				log.Info().Msgf("New deployment detected: %s", deployment.Name)

				deploymentReady(clientset, namespace)

				for i, resource := range conf.Agent {
					name = resource.Name
					err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
					if err != nil {
						log.Error().Msg(err.Error())
						return
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
				log.Info().Msgf("Deployment updated: %s", newDeployment.Name)
				deploymentReady(clientset, namespace)
				for i, resource := range conf.Agent {
					name = resource.Name
					err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
					if err != nil {
						log.Error().Msg(err.Error())
						return
					}
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			// Print the name of the deleted deployment
			deployment, ok := obj.(*appsv1.Deployment)
			if ok {
				log.Info().Msgf("Deployment deleted: %s", deployment.Name)
			}
		},
	})

	// Start the informer
	stopCh := make(chan struct{})
	defer close(stopCh)

	// Run the informer with the stop channel
	deploymentInformer.Run(stopCh)

	// Retrieve the node informer
	nodeInformer := factory.Core().V1().Nodes().Informer()

	// Set up an event handler for when nodes are added or deleted
	_, err = nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			for i, resource := range conf.Agent {
				name = resource.Name
				err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
				if err != nil {
					log.Error().Msg(err.Error())
					return
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			for i, resource := range conf.Agent {
				name = resource.Name
				err = updateAgentResource(clientset, configMap, i, nodesCount, name, namespace)
				if err != nil {
					log.Error().Msg(err.Error())
					return
				}
			}
		},
	})

	// Run the informer with the stop channel
	nodeInformer.Run(stopCh)
}

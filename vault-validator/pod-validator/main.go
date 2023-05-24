package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/exp/slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func cleanUp(cs *kubernetes.Clientset) bool {
	err := cs.CoreV1().Pods("vault").Delete(context.TODO(), "vault-validator", metav1.DeleteOptions{})

	if err != nil {
		fmt.Printf("Error deleting test pod .. %v \n", err)
		return false
	}

	return true
}

func podStatus(cs *kubernetes.Clientset) corev1.PodPhase {
	pod, err := cs.CoreV1().Pods("vault").Get(context.TODO(), "vault-validator", metav1.GetOptions{})

	if err != nil {
		fmt.Printf("Error getting pod status.. %v \n", err)
	}

	return pod.Status.Phase
}

func buildConfigWithContextFromFlags(context string, kubeconfigPath string) (*rest.Config, error) {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{
			CurrentContext: context,
		}).ClientConfig()
}

func main() {
	var kubeconfig *string
	var clusters_to_skip []string
	var cluster_success_validation []string
	var cluster_failed_validation []string

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	clusters := flag.String("clusters", "", "provide a list of clusters for validation (provide 'all' to validate all of available clusters)")
	flag.Parse()

	rawConfig, err := clientcmd.LoadFromFile(*kubeconfig)
	if err != nil {
		panic(err)
	}

	clusters_list := strings.Split(*clusters, ",")

	if clusters_list[0] == "" {
		fmt.Printf("No clusters provided")
		os.Exit(0)
	}

	if clusters_list[0] == "all" {
		for cls := range rawConfig.Contexts {
			clusters_list = append(clusters_list, cls)
		}
		slices.Delete(clusters_list, 0, 1)
	}

	b, err := ioutil.ReadFile("pod.yaml")
	if err != nil {
		log.Fatal(err)
	}

	// jsonBytes, err := json.Marshal(rawConfig.Contexts)
	// fmt.Println(string(jsonBytes))

	fmt.Printf("Clusters: %v \n", clusters_list)

	for _, cluster := range clusters_list {
		var cleanup_status string

		decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(b), 100)
		config, err := buildConfigWithContextFromFlags(cluster, *kubeconfig)
		if err != nil {
			panic(err)
		}

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err.Error())
		}

		dd, err := dynamic.NewForConfig(config)
		if err != nil {
			log.Fatal(err)
		}

		pods, err := clientset.CoreV1().Pods("vault").List(context.TODO(), metav1.ListOptions{LabelSelector: "dh_app=vault"})
		if err != nil {
			err = fmt.Errorf("error getting pods: %v\n", err)
		}

		if len(pods.Items) >= 1 {
			fmt.Printf("Cluster: %v , Spinning up test pod \n", cluster)
			for {
				var rawObj runtime.RawExtension
				if err = decoder.Decode(&rawObj); err != nil {
					break
				}

				obj, gvk, err := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme).Decode(rawObj.Raw, nil, nil)
				unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
				if err != nil {
					log.Fatal(err)
				}

				unstructuredObj := &unstructured.Unstructured{Object: unstructuredMap}

				gr, err := restmapper.GetAPIGroupResources(clientset.Discovery())
				if err != nil {
					log.Fatal(err)
				}

				mapper := restmapper.NewDiscoveryRESTMapper(gr)
				mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
				if err != nil {
					log.Fatal(err)
				}

				var dri dynamic.ResourceInterface
				if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
					if unstructuredObj.GetNamespace() == "" {
						unstructuredObj.SetNamespace("default")
					}
					dri = dd.Resource(mapping.Resource).Namespace(unstructuredObj.GetNamespace())
				} else {
					dri = dd.Resource(mapping.Resource)
				}

				if _, err := dri.Create(context.Background(), unstructuredObj, metav1.CreateOptions{}); err != nil {
					log.Fatal(err)
				}
			}
			if err != io.EOF {
				log.Fatal("eof ", err)
			}

			var pod_status corev1.PodPhase
			var iter int
			for iter <= 10 {
				pod_status = podStatus(clientset)

				fmt.Printf("Cluster: %v iter: %v , Waiting for test pod to come up, Status: %v \n", cluster, iter, pod_status)
				if pod_status == "Running" {
					cluster_success_validation = append(cluster_success_validation, cluster)
					break
				}
				iter++
				time.Sleep(3 * time.Second)
			}

			if pod_status != "Running" {
				cluster_failed_validation = append(cluster_failed_validation, cluster)
			}

			if cleanUp(clientset) {
				cleanup_status = "Done"
			}
			fmt.Printf("Cluster: %v Cleanup: %v \n", cluster, cleanup_status)

		} else {
			fmt.Printf("Cluster: %v , skipping.. \n", cluster)
			clusters_to_skip = append(clusters_to_skip, cluster)
		}

		fmt.Println("\n ## Reporting Status ## \n")
		fmt.Printf("Total Clusters to Validate: %v , Validation Success: %v, Validation Failed: %v, Skipped: %v \n", len(clusters_list), len(cluster_success_validation), len(cluster_failed_validation), len(clusters_to_skip))
	}
}

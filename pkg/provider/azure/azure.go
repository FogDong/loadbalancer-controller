/*
Copyright 2017 Caicloud authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"fmt"
	"strings"
	"time"

	"github.com/caicloud/clientset/informers"
	"github.com/caicloud/clientset/kubernetes"
	lblisters "github.com/caicloud/clientset/listers/loadbalance/v1alpha2"
	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	controllerutil "github.com/caicloud/clientset/util/controller"
	"github.com/caicloud/clientset/util/syncqueue"
	"github.com/caicloud/loadbalancer-controller/pkg/api"
	"github.com/caicloud/loadbalancer-controller/pkg/config"
	"github.com/caicloud/loadbalancer-controller/pkg/plugin"
	"github.com/caicloud/loadbalancer-controller/pkg/toleration"
	lbutil "github.com/caicloud/loadbalancer-controller/pkg/util/lb"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	log "k8s.io/klog"
)

const (
	providerNameSuffix = "-provider-azure"
	providerName       = "azure"
)

type azure struct {
	initialized bool
	image       string

	client kubernetes.Interface
	queue  *syncqueue.SyncQueue

	lbLister  lblisters.LoadBalancerLister
	dLister   appslisters.DeploymentLister
	podLister corelisters.PodLister
}

// New creates a new azure provider plugin
func New() plugin.Interface {
	return &azure{}
}

func (f *azure) Init(cfg config.Configuration, sif informers.SharedInformerFactory) {
	if f.initialized {
		return
	}
	f.initialized = true

	log.Info("Initialize the azure provider")

	// set config
	f.image = cfg.Providers.Azure.Image
	f.client = cfg.Client

	// initialize controller
	lbInformer := sif.Loadbalance().V1alpha2().LoadBalancers()
	dInformer := sif.Apps().V1().Deployments()
	podInfomer := sif.Core().V1().Pods()

	f.lbLister = lbInformer.Lister()
	f.dLister = dInformer.Lister()
	f.podLister = podInfomer.Lister()
	f.queue = syncqueue.NewPassthroughSyncQueue(&lbapi.LoadBalancer{}, f.syncLoadBalancer)

	dInformer.Informer().AddEventHandler(lbutil.NewEventHandlerForDeployment(f.lbLister, f.dLister, f.queue, f.deploymentFiltered))
	podInfomer.Informer().AddEventHandler(lbutil.NewEventHandlerForSyncStatusWithPod(f.lbLister, f.podLister, f.queue, f.podFiltered))
}

func (f *azure) Run(stopCh <-chan struct{}) {

	workers := 1

	if !f.initialized {
		panic("Please initialize provider before you run it")
	}

	defer utilruntime.HandleCrash()

	log.Infof("Starting azure provider, workers %v, image %v", workers, f.image)
	defer log.Info("Shutting down azure provider")

	// lb controller has waited all the informer synced
	// there is no need to wait again here

	defer func() {
		log.Info("Shutting down azure provider")
		f.queue.ShutDown()
	}()

	f.queue.Run(workers)

	<-stopCh
}

func (f *azure) selector(lb *lbapi.LoadBalancer) labels.Set {
	return labels.Set{
		lbapi.LabelKeyCreatedBy: fmt.Sprintf(lbapi.LabelValueFormatCreateby, lb.Namespace, lb.Name),
		lbapi.LabelKeyProvider:  providerName,
	}
}

// filter Deployment that controller does not care
func (f *azure) deploymentFiltered(obj *appsv1.Deployment) bool {
	return f.filteredByLabel(obj)
}

// filter pod that controller does not care
func (f *azure) podFiltered(obj *v1.Pod) bool {
	return f.filteredByLabel(obj)
}

func (f *azure) filteredByLabel(obj metav1.ObjectMetaAccessor) bool {
	// obj.Labels
	selector := labels.Set{lbapi.LabelKeyProvider: providerName}.AsSelector()
	match := selector.Matches(labels.Set(obj.GetObjectMeta().GetLabels()))

	return !match
}

func (f *azure) OnSync(lb *lbapi.LoadBalancer) {
	log.Infof("Syncing providers, triggered by loadbalancer %v/%v", lb.Namespace, lb.Name)
	f.queue.Enqueue(lb)
}

func (f *azure) syncLoadBalancer(obj interface{}) error {
	lb, ok := obj.(*lbapi.LoadBalancer)
	if !ok {
		return fmt.Errorf("expect loadbalancer, got %v", obj)
	}

	key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(lb)

	startTime := time.Now()
	defer func() {
		log.V(5).Infof("Finished syncing azure provider for %v, usedTime %v", key, time.Since(startTime))
	}()

	nlb, err := f.lbLister.LoadBalancers(lb.Namespace).Get(lb.Name)
	if errors.IsNotFound(err) {
		log.Warningf("LoadBalancer %v has been deleted, clean up provider", key)

		return f.cleanup(lb, false)
	}
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to retrieve LoadBalancer %v from store: %v", key, err))
		return err
	}

	// fresh lb
	if lb.UID != nlb.UID {
		return nil
	}
	lb = nlb.DeepCopy()

	if lb.Spec.Providers.Azure == nil {
		// It is not my responsible, clean up legacies
		return f.cleanup(lb, true)
	}

	ds, err := f.getDeploymentsForLoadBalancer(lb)
	if err != nil {
		return err
	}

	if lb.DeletionTimestamp != nil {
		// TODO sync status only
		return nil
	}

	return f.sync(lb, ds)
}

func (f *azure) getDeploymentsForLoadBalancer(lb *lbapi.LoadBalancer) ([]*appsv1.Deployment, error) {

	// construct selector
	selector := f.selector(lb).AsSelector()

	// list all
	dList, err := f.dLister.Deployments(lb.Namespace).List(selector)
	if err != nil {
		return nil, err
	}

	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing deployment (see kubernetes#42639).
	canAdoptFunc := controllerutil.RecheckDeletionTimestamp(func() (metav1.Object, error) {
		// fresh lb
		fresh, err := f.client.LoadbalanceV1alpha2().LoadBalancers(lb.Namespace).Get(lb.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		if fresh.UID != lb.UID {
			return nil, fmt.Errorf("original LoadBalancer %v/%v is gone: got uid %v, wanted %v", lb.Namespace, lb.Name, fresh.UID, lb.UID)
		}
		return fresh, nil
	})

	cm := controllerutil.NewDeploymentControllerRefManager(f.client, lb, selector, api.ControllerKind, canAdoptFunc)
	return cm.Claim(dList)
}

// sync generate desired deployment from lb and compare it with existing deployment
func (f *azure) sync(lb *lbapi.LoadBalancer, dps []*appsv1.Deployment) error {
	desiredDeploy := f.generateDeployment(lb)

	// update
	updated := false

	for _, dp := range dps {
		// two conditions will trigger controller to scale down deployment
		// 1. deployment does not have auto-generated prefix
		// 2. if there are more than one active controllers, there may be many valid deployments.
		//    But we only need one.
		if !strings.HasPrefix(dp.Name, lb.Name+providerNameSuffix) || updated {
			if *dp.Spec.Replicas == 0 {
				continue
			}
			// scale unexpected deployment replicas to zero
			copy := dp.DeepCopy()
			replica := int32(0)
			copy.Spec.Replicas = &replica
			f.client.AppsV1().Deployments(lb.Namespace).Update(copy)
			continue
		}

		updated = true
		if !lbutil.IsStatic(lb) {
			// do not change deployment if the loadbalancer is static
			merged, changed := lbutil.MergeDeployment(dp, desiredDeploy)
			if changed {
				log.Infof("Sync azure deployment %v for lb %v", dp.Name, lb.Name)
				_, err := f.client.AppsV1().Deployments(lb.Namespace).Update(merged)
				if err != nil {
					return err
				}
			}
		}
	}

	// len(dps) == 0 or no deployment's name match desired deployment
	if !updated {
		// create deployment
		log.Infof("Create azure deployment %v for lb %v", desiredDeploy.Name, lb.Name)
		_, err := f.client.AppsV1().Deployments(lb.Namespace).Create(desiredDeploy)
		if err != nil {
			return err
		}
	}

	return f.syncStatus(lb)
}

// cleanup deployment and other resource controlled by azure provider
func (f *azure) cleanup(lb *lbapi.LoadBalancer, deleteStatus bool) error {

	ds, err := f.getDeploymentsForLoadBalancer(lb)
	if err != nil {
		return err
	}

	policy := metav1.DeletePropagationForeground
	gracePeriodSeconds := int64(30)
	for _, d := range ds {
		f.client.AppsV1().Deployments(d.Namespace).Delete(d.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriodSeconds,
			PropagationPolicy:  &policy,
		})
	}

	if deleteStatus {
		return f.deleteStatus(lb)
	}

	return nil
}

func (f *azure) generateDeployment(lb *lbapi.LoadBalancer) *appsv1.Deployment {
	terminationGracePeriodSeconds := int64(300)
	replicas := int32(1)
	t := true

	labels := f.selector(lb)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   lb.Name + providerNameSuffix + "-" + lbutil.RandStringBytesRmndr(5),
			Labels: labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         api.ControllerKind.GroupVersion().String(),
					Kind:               api.ControllerKind.Kind,
					Name:               lb.Name,
					UID:                lb.UID,
					Controller:         &t,
					BlockOwnerDeletion: &t,
				},
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
					// tolerate taints
					Tolerations: toleration.GenerateTolerations(),
					Containers: []v1.Container{
						{
							Name:            providerName,
							Image:           f.image,
							ImagePullPolicy: v1.PullAlways,
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("100m"),
									v1.ResourceMemory: resource.MustParse("50Mi"),
								},
								Limits: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("200m"),
									v1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
							Env: []v1.EnvVar{
								{
									Name: "POD_NAME",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
								{
									Name:  "LOADBALANCER_NAMESPACE",
									Value: lb.Namespace,
								},
								{
									Name:  "LOADBALANCER_NAME",
									Value: lb.Name,
								},
							},
						},
					},
				},
			},
		},
	}

	return deploy
}

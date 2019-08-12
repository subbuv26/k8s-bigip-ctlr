/*-
 * Copyright (c) 2016-2019, F5 Networks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package appmanager

import (
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/F5Networks/k8s-bigip-ctlr/pkg/vlogger"
	"github.com/xeipuuv/gojsonschema"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	v1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	defaultAS3ConfigMapLabel = "f5type in (virtual-server), as3 in (true)"
	svcTenantLabel           = "cis.f5.com/as3-tenant"
	svcAppLabel              = "cis.f5.com/as3-app"
	svcPoolLabel             = "cis.f5.com/as3-pool"
	baseAS3Config            = `{
  "$schema": "https://raw.githubusercontent.com/F5Networks/f5-appsvcs-extension/master/schema/latest/as3-schema-3.11.0-3.json",
  "class": "AS3",
  "declaration": {
    "class": "ADC",
    "schemaVersion": "3.11.0",
    "id": "urn:uuid:ebefe1c6-9629-4339-885f-92492db94120",
    "label": "Basic Declaration",
	"remark": "Default AS3 template"
  }
}
`
	as3SharedApplication = "Shared"
)

var BigIPUsername string
var BigIPPassword string
var BigIPURL string
var BigIPPartitions []string
var as3RC AS3RestClient
var certificates string
var tempAs3ConfigmapDecl as3Declaration
var tempRouteConfigDecl as3ADC

var buffer map[Member]struct{}
var epbuffer map[string]struct{}

// Takes an AS3 Template and perform service discovery with Kubernetes to generate AS3 Declaration
func (appMgr *Manager) processUserDefinedAS3(template string) bool {

	// Validate AS3 Template
	if appMgr.as3Validation == true {
		log.Debugf("[as3] Start validating template")

		if ok := appMgr.validateAS3Template(template); !ok {
			log.Errorf("[as3] Error validating template \n")
			return false
		}
	}

	templateObj := as3Template(template)
	obj, ok := appMgr.getAS3ObjectFromTemplate(templateObj)

	if !ok {
		log.Errorf("[as3] Error processing template\n")
		return false
	}

	buffer = make(map[Member]struct{}, 0)
	epbuffer = make(map[string]struct{}, 0)

	declaration := appMgr.buildAS3Declaration(obj, templateObj)

	appMgr.as3Members = buffer
	appMgr.watchedAS3Endpoints = epbuffer
	tempAs3ConfigmapDecl = declaration
	tempRouteConfigDecl = appMgr.as3RouteCfg
	unifiedDecl, ok := appMgr.getUnifiedAS3Declaration(tempAs3ConfigmapDecl, tempRouteConfigDecl)
	if ok {
		appMgr.postAS3Declaration(unifiedDecl)
	}
	return true
}

// Validates the AS3 Template
func (appMgr *Manager) validateAS3Template(template string) bool {

	var schema = appMgr.schemaLocal + "as3-schema-3.11.0-3-cis.json"

	// Load Both the AS3 Schema and AS3 Template
	schemaLoader := gojsonschema.NewReferenceLoader(schema)
	documentLoader := gojsonschema.NewStringLoader(template)

	result, err := gojsonschema.Validate(schemaLoader, documentLoader)

	if err != nil {
		log.Errorf("%s", err)
		return false
	}

	if !result.Valid() {
		log.Errorf("AS3 Template is not valid. see errors :\n")
		for _, desc := range result.Errors() {
			log.Errorf("- %s\n", desc)
		}
		return false
	}

	log.Debugf("AS3 Template is Validated Successfully \n")
	return true
}

// getAS3ObjectFromTemplate gets an AS3 template as a input parameter.
// It parses AS3 template, constructs an as3Object and returns it.
func (appMgr *Manager) getAS3ObjectFromTemplate(
	template as3Template,
) (as3Object, bool) {
	var tmpl interface{}
	err := json.Unmarshal([]byte(template), &tmpl)
	if err != nil {
		log.Errorf("JSON unmarshal failed: %v\n", err)
		return nil, false
	}

	as3 := make(as3Object)

	// extract as3 declaration from template
	dclr := (tmpl.(map[string]interface{}))["declaration"]
	if dclr == nil {
		log.Error("No ADC class declaration found.")
		return nil, false
	}

	// Loop over all the tenants
	for tn, t := range dclr.(map[string]interface{}) {
		// Filter out non-json values
		if _, ok := t.(map[string]interface{}); !ok {
			continue
		}

		as3[tenantName(tn)] = make(tenant, 0)
		// Loop over all the services in a tenant
		for an, a := range t.(map[string]interface{}) {
			// Filter out non-json values
			if _, ok := a.(map[string]interface{}); !ok {
				continue
			}

			as3[tenantName(tn)][appName(an)] = []poolName{}
			// Loop over all the json objects in an application
			for pn, v := range a.(map[string]interface{}) {
				// Filter out non-json values
				if _, ok := v.(map[string]interface{}); !ok {
					continue
				}

				// filter out non-pool objects
				if cl := getClass(v); cl != "Pool" {
					continue
				}

				// Skip if list of serverAddress is not empty
				mems := (v.(map[string]interface{}))["members"]
				srvAddrs := ((mems.([]interface{}))[0].(map[string]interface{}))["serverAddresses"]
				if len(srvAddrs.([]interface{})) != 0 {
					continue
				}

				//Update the list of pools under corresponding application
				as3[tenantName(tn)][appName(an)] = append(
					as3[tenantName(tn)][appName(an)],
					poolName(pn),
				)
			}
			if len(as3[tenantName(tn)][appName(an)]) == 0 {
				log.Debugf("No pools declared for application: %s,"+
					" tenant: %s\n", an, tn)
			}
		}
		if len(as3[tenantName(tn)]) == 0 {
			log.Debugf("No applications declared for tenant: %s\n", tn)
		}
	}
	if len(as3) == 0 {
		log.Error("No tenants declared in AS3 template")
		return as3, false
	}
	return as3, true
}

func getClass(obj interface{}) string {
	cfg := obj.(map[string]interface{})
	cl, ok := cfg["class"].(string)
	if !ok {
		log.Debugf("No class attribute found")
		return ""
	}
	return cl
}

// Performs Service discovery for the given AS3 Pool and returns a pool.
// Service discovery is loosely coupled with Kubernetes Service labels. A Kubernetes Service is treated as a match for
// an AS3 Pool, if the Kubernetes Service have the following labels and their values matches corresponding AS3
// Object.
// cis.f5.com/as3-tenant=<Tenant Name>
// cis.f5.com/as3-app=<Application Name>
// cis.f5.com/as3-pool=<Pool Name>
// When controller is in NodePort mode, returns a pool of Node IP Address and NodePort.
// When controller is in ClusterIP mode, returns a pool of Cluster IP Address and Service Port. Also, it accumulates
// members for static ARP entry population.
func (appMgr *Manager) getEndpointsForPool(tenant tenantName, app appName, pool poolName) pool {
	log.Debugf("[as3_log] Discovering endpoints for pool: [%v -> %v -> %v]", tenant, app, pool)

	tenantKey := "cis.f5.com/as3-tenant="
	appKey := "cis.f5.com/as3-app="
	poolKey := "cis.f5.com/as3-pool="

	selector := tenantKey + string(tenant) + "," +
		appKey + string(app) + "," +
		poolKey + string(pool)

	svcListOptions := metaV1.ListOptions{
		LabelSelector: selector,
	}

	// Identify services that matches the given label
	services, err := appMgr.kubeClient.CoreV1().Services(v1.NamespaceAll).List(svcListOptions)

	if err != nil {
		log.Errorf("[as3] Error getting service list. %v", err)
		return nil
	}

	var members []Member

	if len(services.Items) > 1 {
		svcNames := ""

		for _, service := range services.Items {
			svcNames += fmt.Sprintf("Service: %v, Namespace: %v \n", service.Name, service.Namespace)
		}

		log.Errorf("[as3] Multiple Services are tagged for this pool. Ignoring all endpoints.\n%v", svcNames)
		return members
	}

	for _, service := range services.Items {
		if appMgr.isNodePort == false { // Controller is in ClusterIP Mode
			endpointsList, err := appMgr.kubeClient.CoreV1().Endpoints(service.Namespace).List(
				metaV1.ListOptions{
					FieldSelector: "metadata.name=" + service.Name,
				},
			)
			if err != nil {
				log.Debugf("[as3] Error getting endpoints for service %v", service.Name)
				continue
			}

			for _, endpoints := range endpointsList.Items {
				for _, subset := range endpoints.Subsets {
					for _, address := range subset.Addresses {
						member := Member{
							Address: address.IP,
							Port:    subset.Ports[0].Port,
						}
						members = append(members, member)

						// Update master AS3 Member list
						buffer[member] = struct{}{}
					}
				}
				// Populate endpoints to watchList
				epbuffer[endpoints.Name] = struct{}{}
			}
		} else { // Controller is in NodePort mode.
			if service.Spec.Type == v1.ServiceTypeNodePort {
				members = appMgr.getEndpointsForNodePort(service.Spec.Ports[0].NodePort)
			} else {
				msg := fmt.Sprintf("Requested service backend '%+v' not of NodePort type", service.Name)
				log.Debug(msg)
			}
		}

		log.Debugf("[as3] Discovered members for service %v is %v", service.Name, members)
	}

	return members
}

// Returns a pool of IP address.
func (appMgr *Manager) getFakeEndpointsForPool(tenant tenantName, app appName, pool poolName) pool {
	return []Member{
		{"1.1.1.1", 80, ""},
		{"2.2.2.2", 80, ""},
		{"3.3.3.3", 80, ""},
	}
}

// Traverses through the AS3 JSON using the information passed from buildAS3Declaration,
// parses the AS3 JSON and populates it with pool members
func updatePoolMembers(tnt tenantName, app appName, pn poolName, ips []string, port int32, templateJSON map[string]interface{}) map[string]interface{} {

	// Get the declaration object from AS3 Json
	dec := (templateJSON["declaration"]).(map[string]interface{})
	// Get the tenant object from AS3 Json
	tet := (dec[string(tnt)]).(map[string]interface{})

	// Continue with the poolName and replace the as3 template with poolMembers
	toName := (tet[string(app)].(map[string]interface{}))
	pool := (toName[string(pn)].(map[string]interface{}))
	poolmem := (((pool["members"]).([]interface{}))[0]).(map[string]interface{})

	// Replace pool member IP addresses
	poolmem["serverAddresses"] = ips
	// Replace port number
	poolmem["servicePort"] = port
	return templateJSON
}

// Takes AS3 template and AS3 Object and produce AS3 Declaration
func (appMgr *Manager) buildAS3Declaration(obj as3Object, template as3Template) as3Declaration {

	var tmp interface{}
	// unmarshall the template of type string to interface
	err := json.Unmarshal([]byte(template), &tmp)
	if nil != err {
		return ""
	}

	// convert tmp to map[string]interface{}, This conversion will help in traversing the as3 object
	templateJSON := tmp.(map[string]interface{})

	// traverse through the as3 object to fetch the list of services and get endpopints using the servicename
	log.Debugf("[as3_log] Started Parsing the AS3 Object")
	for tnt, apps := range obj {
		for app, pools := range apps {
			for _, pn := range pools {
				eps := appMgr.getEndpointsForPool(tnt, app, pn)
				// Handle an empty value
				if len(eps) == 0 {
					continue
				}
				ips := make([]string, 0)
				for _, v := range eps {
					ips = append(ips, v.Address)
				}
				port := eps[0].Port
				log.Debugf("Updating AS3 Template for tenant '%s' app '%s' pool '%s', ", tnt, app, pn)
				updatePoolMembers(tnt, app, pn, ips, port, templateJSON)
			}
		}
	}

	declaration, err := json.Marshal(templateJSON)
	if err != nil {
		log.Errorf("[as3_log] Issue marshalling AS3 Json")
	}
	log.Debugf("[as3_log] AS3 Template is populated with the pool members")

	return as3Declaration(declaration)

}

// Takes AS3 Declaration and post it to BigIP
func (appMgr *Manager) postAS3Declaration(declaration as3Declaration) {
	log.Debugf("[as3_log] Processing AS3 POST call with AS3 Manager")
	as3RC.baseURL = BigIPURL
	_, ok := as3RC.restCallToBigIP("POST", "/mgmt/shared/appsvcs/declare", declaration, appMgr.sslInsecure)
	if ok {
		appMgr.activeCfgMap.Data = string(tempAs3ConfigmapDecl)
		appMgr.as3RouteCfg = tempRouteConfigDecl
	}

}

// Takes AS3 Declaration, method, API route and post it to BigIP
func (as3RestClient *AS3RestClient) restCallToBigIP(method string, route string, declaration as3Declaration, sslInsecure bool) (string, bool) {
	log.Debugf("[as3_log] REST call with AS3 Manager")
	hash := md5.New()
	io.WriteString(hash, string(declaration))
	as3RestClient.newChecksum = string(hash.Sum(nil))
	timeout := time.Duration(60 * time.Second)
	var body []byte
	if as3RestClient.oldChecksum == as3RestClient.newChecksum {
		log.Debugf("[as3_log] No change in declaration.")
		return string(body), true
	}

	//Certificate setting
	// Get the SystemCertPool, continue with an empty pool on error
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	// Get the cert
	certs := []byte(certificates)

	// Append our cert to the system pool
	if ok := rootCAs.AppendCertsFromPEM(certs); !ok {
		log.Debug("[as3_log] No certs appended, using system certs only")
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: sslInsecure,
			RootCAs:            rootCAs,
		},
	}
	as3RestClient.client = &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
	var data io.Reader
	if method == "POST" || method == "PUT" {
		var s = []byte(declaration)
		data = bytes.NewBuffer(s)
	}
	req, err := http.NewRequest(method, as3RestClient.baseURL+route, data)
	if err != nil {
		log.Errorf("[as3_log] Creating new HTTP request error: %v ", err)
		return string(body), false
	}
	req.SetBasicAuth(BigIPUsername, BigIPPassword)
	resp, err := as3RestClient.client.Do(req)
	if err != nil {
		log.Errorf("[as3_log] REST call error: %v ", err)
		return string(body), false
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Errorf("[as3_log] REST call response error: %v ", err)
			return string(body), false
		}
		var response map[string]interface{}
		err = json.Unmarshal([]byte(body), &response)
		if err != nil {
			log.Errorf("[as3_log] Response body unmarshal failed: %v\n", err)
			return string(body), false
		}
		//traverse all response results
		results := (response["results"]).([]interface{})
		for _, value := range results {
			v := value.(map[string]interface{})
			//log result with code, tenant and message
			log.Debugf("[as3_log] Response from Big-IP")
			log.Debugf("[as3_log] code: %v --- tenant:%v --- message: %v", v["code"], v["tenant"], v["message"])
		}
		as3RestClient.oldChecksum = as3RestClient.newChecksum
		return string(body), true
	} else {
		//Other then 200 status code
		log.Errorf("[as3_log] Big-IP Response error %v", resp)
		return string(body), false
	}

}

// Read certificate from configmap
func (appMgr *Manager) getCertFromConfigMap(cfgmap string) {

	certificates = ""
	namespaceCfgmapSlice := strings.Split(cfgmap, "/")
	if len(namespaceCfgmapSlice) < 2 {
		log.Debugf("[as3_log] Invalid trusted-certs-cfgmap option provided.")
	} else {
		certs := ""
		namespace := namespaceCfgmapSlice[0]
		cfgmapName := namespaceCfgmapSlice[1]
		cm, err := appMgr.kubeClient.CoreV1().ConfigMaps(namespace).Get(cfgmapName, metaV1.GetOptions{})
		if err != nil {
			log.Debugf("[as3_log] Reading certificate from configmap error: %v", err)
		} else {
			//Fetching all certificates from configmap
			for _, v := range cm.Data {
				certs = certs + v + "\n"
			}
			certificates = certs
		}
	}
}

// SetupAS3Informers returns an appInformer that includes the following set of informers.
// CfgMapInformer and SvcInformer are label based and endptInformer is not label based.
// These informers are event based informer and do not poll on the resources.
func (appMgr *Manager) SetupAS3Informers() error {
	// resyncPeriod is zero to avoid repolling
	var resyncPeriod time.Duration
	// namespace is Empty to create watchers for all namespaces
	namespace := v1.NamespaceAll

	log.Debug("[as3] Stated creating AS3 Informers")
	cfgMapSelector, err := labels.Parse(defaultAS3ConfigMapLabel)
	if err != nil {
		return fmt.Errorf("Failed to parse AS3 ConfigMap Label Selector string: %v", err)
	}

	defaultSvcLabel := fmt.Sprintf("%v,%v,%v",
		svcTenantLabel,
		svcAppLabel,
		svcPoolLabel,
	)
	svcSelector, err := labels.Parse(defaultSvcLabel)
	if err != nil {
		return fmt.Errorf("Failed to parse Service Label Selector string: %v", err)
	}

	appMgr.as3Informer = &appInformer{
		namespace: namespace,
		stopCh:    make(chan struct{}),
		cfgMapInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1,
				"configmaps",
				namespace,
				cfgMapSelector,
			),
			&v1.ConfigMap{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		svcInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1,
				"services",
				namespace,
				svcSelector,
			),
			&v1.Service{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		endptInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1,
				"endpoints",
				namespace,
				labels.Everything(),
			),
			&v1.Endpoints{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		nodeInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClientv1,
				"nodes",
				namespace,
				labels.Everything(),
			),
			&v1.Node{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
	}

	appMgr.as3Informer.cfgMapInformer.AddEventHandler(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueAS3ConfigMap(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueAS3ConfigMap(cur) },
		},
	)
	appMgr.as3Informer.svcInformer.AddEventHandler(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueAS3Service(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueAS3Service(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueAS3Service(obj) },
		},
	)
	appMgr.as3Informer.endptInformer.AddEventHandler(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueAS3Endpoints(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueAS3Endpoints(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueAS3Endpoints(obj) },
		},
	)
	appMgr.as3Informer.nodeInformer.AddEventHandler(
		&cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) { appMgr.enqueueNode(obj) },
			// TODO: Need to add handler for update event
			// UpdateFunc: func(old, cur interface{}) { appMgr.enqueueNode(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueNode(obj) },
		},
	)

	return nil
}

func (appMgr *Manager) enqueueAS3ConfigMap(obj interface{}) {
	if ok, keys := appMgr.checkValidAS3ConfigMap(obj); ok {
		for _, key := range keys {
			appMgr.vsQueue.Add(*key)
		}
	}
}

func (appMgr *Manager) enqueueAS3Service(obj interface{}) {
	if ok, keys := appMgr.checkValidAS3Service(obj); ok {
		for _, key := range keys {
			appMgr.vsQueue.Add(*key)
		}
	}
}

func (appMgr *Manager) enqueueAS3Endpoints(obj interface{}) {
	if ok, keys := appMgr.checkValidAS3Endpoints(obj); ok {
		for _, key := range keys {
			appMgr.vsQueue.Add(*key)
		}
	}
}

func (appMgr *Manager) checkValidAS3ConfigMap(obj interface{}) (
	bool, []*serviceQueueKey) {

	var keyList []*serviceQueueKey
	cm := obj.(*v1.ConfigMap)
	namespace := cm.ObjectMeta.Namespace

	log.Debugf("[as3_log] Found AS3 ConfigMap - %s.", cm.ObjectMeta.Name)
	key := &serviceQueueKey{
		Namespace: namespace,
		AS3Name:   cm.ObjectMeta.Name,
		AS3Data:   cm.Data["template"],
	}
	keyList = append(keyList, key)
	return true, keyList
}

func (appMgr *Manager) checkValidAS3Service(obj interface{}) (
	bool, []*serviceQueueKey) {

	if appMgr.activeCfgMap.Name == "" {
		return false, nil
	}

	var keyList []*serviceQueueKey
	svc := obj.(*v1.Service)
	namespace := svc.ObjectMeta.Namespace

	key := &serviceQueueKey{
		ServiceName: svc.ObjectMeta.Name,
		Namespace:   namespace,
		AS3Name:     appMgr.activeCfgMap.Name,
		AS3Data:     appMgr.activeCfgMap.Data,
	}
	keyList = append(keyList, key)
	return true, keyList
}

func (appMgr *Manager) checkValidAS3Endpoints(obj interface{}) (
	bool, []*serviceQueueKey) {

	if appMgr.activeCfgMap.Name == "" {
		return false, nil
	}
	eps := obj.(*v1.Endpoints)

	if _, ok := appMgr.watchedAS3Endpoints[eps.Name]; !ok {
		return false, nil
	}

	var keyList []*serviceQueueKey
	namespace := eps.ObjectMeta.Namespace

	key := &serviceQueueKey{
		ServiceName: eps.ObjectMeta.Name,
		Namespace:   namespace,
		AS3Name:     appMgr.activeCfgMap.Name,
		AS3Data:     appMgr.activeCfgMap.Data,
	}
	keyList = append(keyList, key)
	return true, keyList
}

func (appMgr *Manager) getUnifiedAS3Declaration(as3ConfigmapDecl as3Declaration, routeConfigDecl as3ADC) (as3Declaration, bool) {
	if routeConfigDecl == nil && string(as3ConfigmapDecl) == "" {
		// return false if empty routeConfigDecl and as3ConfigmapDecl
		return as3Declaration(as3ConfigmapDecl), false
	}

	// Need to process Routes
	var as3Config map[string]interface{}
	if as3ConfigmapDecl != "" {
		// Merge activeCfgMap and as3RouteCfg
		_ = json.Unmarshal([]byte(as3ConfigmapDecl), &as3Config)
	} else {
		// Merge base AS3 template and as3RouteCfg
		_ = json.Unmarshal([]byte(baseAS3Config), &as3Config)
	}
	adc := as3Config["declaration"].(map[string]interface{})

	for k, v := range routeConfigDecl {
		adc[k] = v
	}

	unifiedDecl, _ := json.Marshal(as3Config)
	log.Debugf("as3_log: Unified AS3 Declaration: %v\n", string(unifiedDecl))
	return as3Declaration(string(unifiedDecl)), true
}

func (appMgr *Manager) postRouteDeclarationHost() {
	adc := appMgr.generateAS3RouteDeclaration()
	//Get unified declaration
	tempAs3ConfigmapDecl = as3Declaration(appMgr.activeCfgMap.Data)
	tempRouteConfigDecl = adc
	unifiedDecl, ok := appMgr.getUnifiedAS3Declaration(tempAs3ConfigmapDecl, tempRouteConfigDecl)
	if ok {
		appMgr.postAS3Declaration(unifiedDecl)
	}
}

func (appMgr *Manager) generateAS3RouteDeclaration() as3ADC {
	// Create Shared as3Application object
	sharedApp := as3Application{}
	sharedApp["class"] = "Application"
	sharedApp["template"] = "shared"

	// Process CIS Resources to create AS3 Resources
	appMgr.processResourcesForAS3(sharedApp)

	// Process DataGroup to be consumed by IRule
	appMgr.processDataGroupForAS3(sharedApp)

	// Process CustomProfiles
	appMgr.processCustomeProfilesForAS3(sharedApp)

	// Process RouteProfiles
	appMgr.processRouteProfilesForAS3(sharedApp)

	// Process IRules
	appMgr.processIRulesForAS3(sharedApp)

	// Create AS3 Tenant
	tenant := as3Tenant{
		"class":              "Tenant",
		as3SharedApplication: sharedApp,
	}
	as3JSONDecl := as3ADC{
		DEFAULT_PARTITION: tenant,
	}
	return as3JSONDecl
}

func (appMgr *Manager) processResourcesForAS3(sharedApp as3Application) {
	for _, cfg := range appMgr.resources.GetAllResources() {
		//Create policies
		createPoliciesDecl(cfg, sharedApp)

		//Create health monitor declaration
		createMonitorDecl(cfg, sharedApp)

		//Create pools
		createPoolDecl(cfg, sharedApp)

		//Create AS3 Service for virtual server
		createServiceDecl(cfg, sharedApp)
	}
}

func (appMgr *Manager) processIRulesForAS3(sharedApp as3Application) {
	// Create passthrough irule declaration
	for _, v := range appMgr.irulesMap {
		iRule := &as3IRules{}
		iRule.Class = "iRule"
		iRule.IRule = v.Code
		sharedApp[as3FormatedString(v.Name)] = iRule
	}
}

func (appMgr *Manager) processCustomeProfilesForAS3(sharedApp as3Application) {
	caBundleName := "serverssl_ca_bundle"
	clientTLSCreated := false
	// TLS Certificates are available in CustomProfiles
	for key, prof := range appMgr.customProfiles.profs {
		// Create TLSServer and Certificate for each profile
		svcName := as3FormatedString(key.ResourceName)
		if svcName == "" {
			continue
		}
		if ok := createUpdateTLSServer(prof, svcName, sharedApp); ok {
			// Create Certificate only if the corresponding TLSServer got created
			createCertificateDecl(prof, sharedApp)
		} else {
			createUpdateCABundle(prof, caBundleName, sharedApp)
			if !clientTLSCreated {
				validateCertificate := false
				skey := secretKey{
					Name: prof.Name + "-ca",
				}
				if _, ok := appMgr.customProfiles.profs[skey]; ok {
					validateCertificate = true
				}
				clientTLSCreated = createTLSClient(prof, svcName, caBundleName, validateCertificate, sharedApp)
			}
		}
	}
}

func (appMgr *Manager) processRouteProfilesForAS3(sharedApp as3Application) {
	// Processes RouteProfs to create AS3 Declaration for Route annotations
	// Override/Set ServerTLS/ClientTLS in AS3 Service as annotation takes higher priority

	for svcName, cfg := range appMgr.resources.rsMap {
		svc, ok := sharedApp[as3FormatedString(svcName)].(*as3Service)
		if !ok {
			continue
		}
		for key, val := range cfg.MetaData.RouteProfs {
			if key.Context == customProfileClient {
				// Incoming traffic (clientssl) from a web client will be handled by ServerTLS in AS3
				svc.ServerTLS = &as3ResourcePointer{
					BigIP: val,
				}
			} else if key.Context == customProfileServer {
				// Outgoing traffic (serverssl) to BackEnd Servers from BigIP will be handled by ClientTLS in AS3
				svc.ClientTLS = &as3ResourcePointer{
					BigIP: val,
				}
			}
		}
	}
}

func (appMgr *Manager) processDataGroupForAS3(sharedApp as3Application) {
	for _, idg := range appMgr.intDgMap {
		for _, dg := range idg {
			dgMap := &as3SSLPassthroughServernameDg{}
			dgMap.Class = "Data_Group"
			dgMap.KeyDataType = "string"
			for _, record := range dg.Records {
				var rec as3Record
				rec.Key = record.Name
				rec.Value = as3FormatedString(record.Data)
				dgMap.Records = append(dgMap.Records, rec)
			}
			sharedApp[as3FormatedString(dg.Name)] = dgMap
		}
	}
}

func createPoliciesDecl(cfg *ResourceConfig, sharedApp as3Application) {
	for _, pl := range cfg.Policies {
		//Create EndpointPolicy
		ep := &as3EndpointPolicy{}
		for _, rl := range pl.Rules {

			ep.Class = "Endpoint_Policy"
			s := strings.Split(pl.Strategy, "/")
			ep.Strategy = s[len(s)-1]

			//Create rules
			rulesData := &as3Rule{Name: as3FormatedString(rl.Name)}

			//Create condition object
			createRouteRuleCondition(rl, rulesData)

			//Creat action object
			createRouteRuleAction(rl, rulesData)

			ep.Rules = append(ep.Rules, *rulesData)
		}
		//Setting Endpoint_Policy Name
		sharedApp[as3FormatedString(pl.Name)] = ep
	}
}

// Create AS3 Pools for Route
func createPoolDecl(cfg *ResourceConfig, sharedApp as3Application) {
	for _, v := range cfg.Pools {
		pool := &as3Pool{}
		pool.LoadBalancingMode = v.Balance
		pool.Class = "Pool"
		for _, val := range v.Members {
			var member as3PoolMember
			member.AddressDiscovery = "static"
			member.ServicePort = val.Port
			member.ServerAddresses = append(member.ServerAddresses, val.Address)
			pool.Members = append(pool.Members, member)
		}
		for _, val := range v.MonitorNames {
			var monitor as3ResourcePointer
			use := strings.Split(val, "/")
			monitor.Use = fmt.Sprintf("/%s/%s/%s",
				DEFAULT_PARTITION,
				as3SharedApplication,
				as3FormatedString(use[2]),
			)
			pool.Monitors = append(pool.Monitors, monitor)
		}
		sharedApp[as3FormatedString(v.Name)] = pool
	}
}

func isSecuredVirtualServer(policies []nameRef) bool {
	for _, p := range policies {
		if p.Name == "openshift_secure_routes" {
			return true
		}
	}
	return false
}

// Create AS3 Service for Route
func createServiceDecl(cfg *ResourceConfig, sharedApp as3Application) {
	svc := &as3Service{}

	if len(cfg.Virtual.Policies) == 1 {
		svc.PolicyEndpoint = fmt.Sprintf("/%s/%s/%s",
			DEFAULT_PARTITION,
			as3SharedApplication,
			cfg.Virtual.Policies[0].Name,
		)
	} else if len(cfg.Virtual.Policies) > 1 {
		var peps []as3ResourcePointer
		for _, pep := range cfg.Virtual.Policies {
			svc.PolicyEndpoint = append(
				peps,
				as3ResourcePointer{
					BigIP: fmt.Sprintf("/%s/%s/%s",
						DEFAULT_PARTITION,
						as3SharedApplication,
						pep.Name,
					),
				},
			)
		}
		svc.PolicyEndpoint = peps
	}

	svc.Layer4 = cfg.Virtual.IpProtocol
	svc.Source = "0.0.0.0/0"
	svc.TranslateServerAddress = true
	svc.TranslateServerPort = true

	if isSecuredVirtualServer(cfg.Virtual.Policies) {
		svc.Class = "Service_HTTPS"
		redirect80 := false
		svc.Redirect80 = &redirect80
	} else {
		svc.Class = "Service_HTTP"
	}

	for _, prof := range cfg.Virtual.Profiles {
		switch prof.Name {
		case "http":
			svc.ProfileHTTP = as3ResourcePointer{
				BigIP: fmt.Sprintf("/%s/%s", prof.Partition, prof.Name),
			}
		case "tcp":
			svc.ProfileTCP = as3ResourcePointer{
				BigIP: fmt.Sprintf("/%s/%s", prof.Partition, prof.Name),
			}
		}
	}

	destination := strings.Split(cfg.Virtual.Destination, "/")
	ipPort := strings.Split(destination[len(destination)-1], ":")
	va := append(svc.VirtualAddresses, ipPort[0])
	svc.VirtualAddresses = va
	port, _ := strconv.Atoi(ipPort[1])
	svc.VirtualPort = port
	svc.SNAT = "auto"
	for _, v := range cfg.Virtual.IRules {
		s := strings.Split(v, "/")
		svc.IRules = append(svc.IRules, as3FormatedString(s[len(s)-1]))
	}

	sharedApp[as3FormatedString(cfg.Virtual.Name)] = svc
}

// Create AS3 Rule Condition for Route
func createRouteRuleCondition(rl *Rule, rulesData *as3Rule) {
	for _, c := range rl.Conditions {
		var condition as3Condition
		if c.Host {
			condition.Name = "host"
			condition.All = &as3PolicyCompareString{
				Values: c.Values,
			}
			if c.HTTPHost {
				condition.Type = "httpHeader"
			}
			if c.Equals {
				condition.All.Operand = "equals"
			}
		} else if c.PathSegment {
			condition.PathSegment = &as3PolicyCompareString{
				Values: c.Values,
			}
			if c.Name != "" {
				condition.Name = c.Name
			}
			condition.Index = c.Index
			if c.HTTPURI {
				condition.Type = "httpUri"
			}
			if c.Equals {
				condition.PathSegment.Operand = "equals"
			}
		}

		if c.Request {
			condition.Event = "request"
		}

		rulesData.Conditions = append(rulesData.Conditions, condition)
	}
}

// Create AS3 Rule Action for Route
func createRouteRuleAction(rl *Rule, rulesData *as3Rule) {
	var action as3Action
	for _, v := range rl.Actions {
		if v.Forward {
			action.Type = "forward"
		}
		if v.Request {
			action.Event = "request"
		}

		p := strings.Split(v.Pool, "/")
		action.Select = &as3ActionForwardSelect{
			Pool: &as3ResourcePointer{
				Use: as3FormatedString(p[len(p)-1]),
			},
		}
	}
	rulesData.Actions = append(rulesData.Actions, action)
}

//Create health monitor declaration
func createMonitorDecl(cfg *ResourceConfig, sharedApp as3Application) {

	for _, v := range cfg.Monitors {
		monitor := &as3Monitor{}
		monitor.Class = "Monitor"
		monitor.Interval = v.Interval
		monitor.MonitorType = v.Type
		monitor.Timeout = v.Timeout
		val := 0
		monitor.TargetPort = &val
		targetAddressStr := ""
		monitor.TargetAddress = &targetAddressStr
		//Monitor type
		switch v.Type {
		case "http":
			adaptiveFalse := false
			monitor.Adaptive = &adaptiveFalse
			monitor.Dscp = &val
			monitor.Receive = "none"
			if v.Recv != "" {
				monitor.Receive = v.Recv
			}
			monitor.TimeUnitilUp = &val
			monitor.Send = v.Send
		case "https":
			//Todo: For https monitor type
			adaptiveFalse := false
			monitor.Adaptive = &adaptiveFalse
		}
		sharedApp[as3FormatedString(v.Name)] = monitor
	}

}

//Replacing "-" with "_" for given string
func as3FormatedString(str string) string {
	return strings.Replace(str, "-", "_", -1)
}

func createUpdateCABundle(prof CustomProfile, caBundleName string, sharedApp as3Application) {
	// For TLSClient only Cert (DestinationCACertificate) is given and key is empty string
	if "" != prof.Cert && "" == prof.Key {
		caBundle, ok := sharedApp[caBundleName].(*as3CABundle)

		if !ok {
			caBundle = &as3CABundle{
				Class:  "CA_Bundle",
				Bundle: "",
			}
			sharedApp[caBundleName] = caBundle
		}
		caBundle.Bundle += "\n" + prof.Cert
	}
}

func createCertificateDecl(prof CustomProfile, sharedApp as3Application) {
	if "" != prof.Cert && "" != prof.Key {
		cert := &as3Certificate{
			Class:       "Certificate",
			Certificate: prof.Cert,
			PrivateKey:  prof.Key,
			ChainCA:     prof.CAFile,
		}

		sharedApp[as3FormatedString(prof.Name)] = cert
	}
}

func createUpdateTLSServer(prof CustomProfile, svcName string, sharedApp as3Application) bool {
	if "" != prof.Cert && "" != prof.Key {
		svc := sharedApp[svcName].(*as3Service)
		tlsServerName := fmt.Sprintf("%s_tls_server", svcName)
		certName := as3FormatedString(prof.Name)

		tlsServer, ok := sharedApp[tlsServerName].(*as3TLSServer)
		if !ok {
			tlsServer = &as3TLSServer{
				Class:        "TLS_Server",
				Certificates: []as3TLSServerCertificates{},
			}

			sharedApp[tlsServerName] = tlsServer
			svc.ServerTLS = tlsServerName
		}

		tlsServer.Certificates = append(
			tlsServer.Certificates,
			as3TLSServerCertificates{
				Certificate: certName,
			},
		)
		return true
	}
	return false
}

func createTLSClient(
	prof CustomProfile,
	svcName, caBundleName string,
	validateCertificate bool,
	sharedApp as3Application,
) bool {
	// For TLSClient only Cert (DestinationCACertificate) is given and key is empty string
	if "" != prof.Cert && "" == prof.Key {
		svc := sharedApp[svcName].(*as3Service)
		tlsClientName := fmt.Sprintf("%s_tls_client", svcName)

		tlsClient := &as3TLSClient{
			Class:               "TLS_Client",
			ValidateCertificate: validateCertificate,
		}
		tlsClient.TrustCA = &as3ResourcePointer{
			Use: caBundleName,
		}
		sharedApp[tlsClientName] = tlsClient
		svc.ClientTLS = tlsClientName

		return true
	}
	return false
}

//DeleteAs3ManagedConfiguration as3 managed partitioned configurations when switching back to agent cccl from as3
func (appMgr *Manager) DeleteAs3ManagedConfiguration() {
	var as3Config map[string]interface{}
	if appMgr.agent == "cccl" {
		_ = json.Unmarshal([]byte(baseAS3Config), &as3Config)
		m := as3Config["declaration"].(map[string]interface{})

		for i := range BigIPPartitions {
			log.Debugf("as3_log: Deleting configuration from partition %v\n", BigIPPartitions[i]+"_AS3")
			m[BigIPPartitions[i]+"_AS3"] = map[string]string{"class": "Tenant"}
			data, _ := json.Marshal(as3Config)
			appMgr.postAS3Declaration(as3Declaration(data))
		}
	}
}

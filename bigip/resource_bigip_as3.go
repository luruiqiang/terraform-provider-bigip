/*
Copyright 2019 F5 Networks Inc.
This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0.
If a copy of the MPL was not distributed with this file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package bigip

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"

	bigip "github.com/f5devcentral/go-bigip"
	"github.com/f5devcentral/go-bigip/f5teem"
	uuid "github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/structure"
)

var x = 0
var m sync.Mutex
var createdTenants string

func resourceBigipAs3() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceBigipAs3Create,
		ReadContext:   resourceBigipAs3Read,
		UpdateContext: resourceBigipAs3Update,
		DeleteContext: resourceBigipAs3Delete,
		Importer: &schema.ResourceImporter{
			StateContext: func(ctx context.Context, d *schema.ResourceData, m interface{}) ([]*schema.ResourceData, error) {
				// d.Id() here is the last argument passed to the `terraform import RESOURCE_TYPE.RESOURCE_NAME RESOURCE_ID` command
				// Here we use a function to parse the import ID (like the example above) to simplify our logic

				_ = d.Set("tenant_list", d.Id())
				_ = d.Set("tenant_filter", d.Id())

				return []*schema.ResourceData{d}, nil
			},
		},
		Schema: map[string]*schema.Schema{
			"as3_json": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "AS3 json",
				StateFunc: func(v interface{}) string {
					jsonString, _ := structure.NormalizeJsonString(v)
					return jsonString
				},
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					oldResp := []byte(old)
					newResp := []byte(new)
					oldJsonref := make(map[string]interface{})
					newJsonref := make(map[string]interface{})
					_ = json.Unmarshal(oldResp, &oldJsonref)
					_ = json.Unmarshal(newResp, &newJsonref)
					jsonEqualityBefore := reflect.DeepEqual(oldJsonref, newJsonref)
					if jsonEqualityBefore {
						return true
					}
					for key, value := range oldJsonref {
						if rec, ok := value.(map[string]interface{}); ok && key == "declaration" {
							for range rec {
								delete(rec, "updateMode")
								delete(rec, "schemaVersion")
								delete(rec, "id")
								delete(rec, "label")
								delete(rec, "remark")
							}
						}
					}
					for key, value := range newJsonref {
						if rec, ok := value.(map[string]interface{}); ok && key == "declaration" {
							for range rec {
								delete(rec, "updateMode")
								delete(rec, "schemaVersion")
								delete(rec, "id")
								delete(rec, "label")
								delete(rec, "remark")
							}
						}
					}

					ignoreMetadata := d.Get("ignore_metadata").(bool)
					jsonEqualityAfter := reflect.DeepEqual(oldJsonref, newJsonref)
					if ignoreMetadata {
						if jsonEqualityAfter {
							return true
						} else {
							return false
						}

					} else {
						if !jsonEqualityBefore {
							return false
						}
					}
					return true
				},
				ValidateFunc: func(v interface{}, k string) (ws []string, errors []error) {
					if _, err := structure.NormalizeJsonString(v); err != nil {
						errors = append(errors, fmt.Errorf("%q contains an invalid JSON: %s", k, err))
					}
					as3json := v.(string)
					resp := []byte(as3json)
					jsonRef := make(map[string]interface{})
					_ = json.Unmarshal(resp, &jsonRef)
					for key, value := range jsonRef {
						if key == "class" && value != "AS3" {
							errors = append(errors, fmt.Errorf("JSON must have AS3 class"))
						}
						if rec, ok := value.(map[string]interface{}); ok && key == "declaration" {
							for k, v := range rec {
								if k == "class" && v != "ADC" {
									errors = append(errors, fmt.Errorf("JSON must have ADC class"))
								}
							}
						}
					}
					return
				},
			},
			"ignore_metadata": {
				Type:        schema.TypeBool,
				Optional:    true,
				Description: "Set True if you want to ignore metadata update",
				Default:     false,
			},
			"tenant_name": {
				Type:        schema.TypeString,
				Optional:    true,
				Deprecated:  "this attribute is no longer in use",
				Description: "Name of Tenant",
			},
			"tenant_filter": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "Name of Tenant",
			},
			"tenant_list": {
				Type:        schema.TypeString,
				Computed:    true,
				Optional:    true,
				Description: "Name of Tenant",
			},
			"application_list": {
				Type:        schema.TypeString,
				Computed:    true,
				Optional:    true,
				Description: "Name of Application",
			},
			"task_id": {
				Type:        schema.TypeString,
				Computed:    true,
				Optional:    true,
				Description: "ID of AS3 post declaration async task",
			},
		},
	}
}

func resourceBigipAs3Create(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*bigip.BigIP)
	as3Json := d.Get("as3_json").(string)
	m.Lock()
	defer m.Unlock()
	log.Printf("[INFO] Creating As3 config")
	tenantFilter := d.Get("tenant_filter").(string)
	tenantList, _, applicationList := client.GetTenantList(as3Json)
	tenantCount := strings.Split(tenantList, ",")
	if tenantFilter != "" {
		log.Printf("[DEBUG] tenantFilter:%+v", tenantFilter)
		if !contains(tenantCount, tenantFilter) {
			return diag.FromErr(fmt.Errorf("tenant_filter: (%s) not exist in as3_json provided ", tenantFilter))
		}
		tenantList = tenantFilter
	}
	_ = d.Set("tenant_list", tenantList)
	_ = d.Set("application_list", applicationList)
	strTrimSpace, err := client.AddTeemAgent(as3Json)
	if err != nil {
		return diag.FromErr(err)
	}
	log.Printf("[INFO] Creating as3 config in bigip:%s", strTrimSpace)
	err, successfulTenants, taskID := client.PostAs3Bigip(strTrimSpace, tenantList)
	log.Printf("[DEBUG] successfulTenants :%+v", successfulTenants)
	if err != nil {
		if successfulTenants == "" {
			return diag.FromErr(fmt.Errorf("posting as3 config failed for tenants:(%s) with error: %v", tenantList, err))
		}
		_ = d.Set("tenant_list", successfulTenants)
		if len(successfulTenants) != len(tenantList) {
			return diag.FromErr(err)
		}
	}
	if !client.Teem {
		id := uuid.New()
		uniqueID := id.String()
		assetInfo := f5teem.AssetInfo{
			Name:    "Terraform-provider-bigip",
			Version: client.UserAgent,
			Id:      uniqueID,
		}
		apiKey := os.Getenv("TEEM_API_KEY")
		teemDevice := f5teem.AnonymousClient(assetInfo, apiKey)
		f := map[string]interface{}{
			"Number_of_tenants": len(tenantCount),
			"Terraform Version": client.UserAgent,
		}
		tsVer := strings.Split(client.UserAgent, "/")
		err = teemDevice.Report(f, "bigip_as3", tsVer[3])
		if err != nil {
			log.Printf("[ERROR]Sending Telemetry data failed:%v", err)
		}
	}
	log.Printf("[DEBUG] ID for resource :%+v", d.Get("tenant_list").(string))
	_ = d.Set("task_id", taskID)
	if d.Get("tenant_list").(string) != "" {
		d.SetId(d.Get("tenant_list").(string))
	} else {
		d.SetId("Common")
	}
	createdTenants = d.Get("tenant_list").(string)
	x++
	log.Printf("[TRACE] %+v", x)
	return resourceBigipAs3Read(ctx, d, meta)
}
func resourceBigipAs3Read(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*bigip.BigIP)
	log.Printf("[INFO] Reading As3 config")
	var name string
	var tList string
	as3Json := d.Get("as3_json").(string)

	if d.Get("as3_json") != nil {
		tList, _, _ = client.GetTenantList(as3Json)
		if createdTenants != "" && createdTenants != tList {
			tList = createdTenants
		}
	}
	if d.Id() != "" && tList != "" {
		name = tList
	} else {
		name = d.Id()
	}

	applicationList := d.Get("application_list").(string)
	log.Printf("[DEBUG] Tenants in AS3 get call : %s", name)
	if name != "" {
		as3Resp, err := client.GetAs3(name, applicationList)
		log.Printf("[DEBUG] AS3 json retreived from the GET call in Read function : %s", as3Resp)
		if err != nil {
			log.Printf("[ERROR] Unable to retrieve json ")
			if err.Error() == "unexpected end of JSON input" {
				log.Printf("[ERROR] %v", err)
				return nil
			}
			d.SetId("")
			return diag.FromErr(err)
		}
		if as3Resp == "" {
			log.Printf("[WARN] Json (%s) not found, removing from state", d.Id())
			_ = d.Set("as3_json", "")
			// d.SetId("")
			return nil
		}
		_ = d.Set("as3_json", as3Resp)
		_ = d.Set("tenant_list", name)
	} else if d.Get("task_id") != nil {
		taskResponse, err := client.Getas3TaskResponse(d.Get("task_id").(string))
		if err != nil {
			d.SetId("")
			return nil
		}
		_ = d.Set("as3_json", taskResponse)
		_ = d.Set("tenant_list", name)
	}
	return nil
}

func resourceBigipAs3Update(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*bigip.BigIP)
	as3Json := d.Get("as3_json").(string)
	m.Lock()
	defer m.Unlock()
	log.Printf("[INFO] Updating As3 Config :%s", as3Json)
	tenantList, _, _ := client.GetTenantList(as3Json)
	oldTenantList := d.Get("tenant_list").(string)
	tenantFilter := d.Get("tenant_filter").(string)
	if tenantFilter == "" {
		if tenantList != oldTenantList {
			_ = d.Set("tenant_list", tenantList)
			newList := strings.Split(tenantList, ",")
			oldList := strings.Split(oldTenantList, ",")
			deletedTenants := client.TenantDifference(oldList, newList)
			if deletedTenants != "" {
				err, _ := client.DeleteAs3Bigip(deletedTenants)
				if err != nil {
					log.Printf("[ERROR] Unable to Delete removed tenants: %v :", err)
					return diag.FromErr(err)
				}
			}
		}
	} else {
		if !contains(strings.Split(tenantList, ","), tenantFilter) {
			log.Printf("[WARNING]tenant_filter: (%s) not exist in as3_json provided ", tenantFilter)
		} else {
			tenantList = tenantFilter
		}
	}
	strTrimSpace, err := client.AddTeemAgent(as3Json)
	if err != nil {
		return diag.FromErr(err)
	}
	err, successfulTenants, taskID := client.PostAs3Bigip(strTrimSpace, tenantList)
	log.Printf("[DEBUG] successfulTenants :%+v", successfulTenants)
	if err != nil {
		if successfulTenants == "" {
			return diag.FromErr(fmt.Errorf("Error updating json  %s: %v", tenantList, err))
		}
		_ = d.Set("tenant_list", successfulTenants)
		if len(successfulTenants) != len(tenantList) {
			return diag.FromErr(err)
		}
	}
	createdTenants = d.Get("tenant_list").(string)
	_ = d.Set("task_id", taskID)
	x++
	return resourceBigipAs3Read(ctx, d, meta)
}

func resourceBigipAs3Delete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*bigip.BigIP)
	m.Lock()
	defer m.Unlock()
	log.Printf("[INFO] Deleting As3 config")
	var name string
	var tList string

	if d.Get("as3_json") != nil {
		tList, _, _ = client.GetTenantList(d.Get("as3_json").(string))
	}

	if d.Id() != "" && tList != "" {
		name = tList
	} else {
		name = d.Id()
	}
	err, failedTenants := client.DeleteAs3Bigip(name)
	if err != nil {
		log.Printf("[ERROR] Unable to DeleteContext: %v :", err)
		return diag.FromErr(err)
	}
	if failedTenants != "" {
		_ = d.Set("tenant_list", name)
		return resourceBigipAs3Read(ctx, d, meta)
	}
	x++
	d.SetId("")
	return nil
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

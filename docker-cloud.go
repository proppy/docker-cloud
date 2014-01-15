//
// Copyright (C) 2013 The Docker Cloud authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/proppy/docker-cloud/dockercloud"
)

var (
	dockerPort   = flag.Int("dockerport", 8000, "The remote port to run docker on")
	tunnelPort   = flag.Int("tunnelport", 8001, "The local port open the tunnel to docker")
	instanceName = flag.String("instancename", "docker-instance", "The name of the instance")
	zone         = flag.String("zone", "us-central1-a", "The zone to run in")
)

type DockerCloud struct {
	dockercloud.Cloud
}

func (cloud *DockerCloud) GetOrCreateInstance() (string, error) {
	ip, err := cloud.GetPublicIPAddress(*instanceName, *zone)
	instanceRunning := len(ip) > 0
	if instanceRunning {
		return ip, err
	}

	// Otherwise create a new VM.
	return cloud.CreateInstance(*instanceName, *zone)
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: docker-cloud start|stop")
		flag.Usage()
		flag.PrintDefaults()
		os.Exit(-1)
	}
	cloud := DockerCloud{dockercloud.NewCloudGce()}
	switch args[0] {
	case "start":
		_, err := cloud.GetOrCreateInstance()
		if err != nil {
			log.Fatalf("failed to create VM instance")
		}
		_, err = cloud.OpenSecureTunnel(*instanceName, *zone, *tunnelPort, *dockerPort)
		if err != nil {
			log.Fatalf("failed to create SSH tunnel")
		}
		var c chan bool
		<-c
	case "stop":
		err := cloud.DeleteInstance(*instanceName, *zone)
		if err != nil {
			log.Fatalf("failed to delete VM instance")
		}
	}
}

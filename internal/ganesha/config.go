/*
Copyright 2026.

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

// Package ganesha renders the NFS-Ganesha server configuration used by the
// per-SharedVolume server Deployment. NFSv4.1/4.2 only: a single TCP port
// (2049) with no rpcbind/mountd/statd, which is what makes the server viable
// inside an unprivileged pod.
package ganesha

// Config is the ganesha.conf for a single-export NFSv4 server. It is static
// today; it lives behind a function so per-volume options (squash, ACLs,
// pseudo paths) can be threaded through later without touching callers.
func Config() string {
	return `NFS_CORE_PARAM {
    NFS_Port = 2049;
    Protocols = 4;
    Enable_NLM = false;
    Enable_RQUOTA = false;
    mount_path_pseudo = true;
}

NFSV4 {
    Minor_Versions = 1, 2;
    Grace_Period = 15;
    Lease_Lifetime = 15;
    RecoveryBackend = fs;
}

EXPORT {
    Export_Id = 1;
    Path = /export;
    Pseudo = /;
    Protocols = 4;
    Transports = TCP;
    Access_Type = RW;
    Squash = No_Root_Squash;
    SecType = sys;
    Disable_ACL = true;
    FSAL { Name = VFS; }
}

LOG { Default_Log_Level = EVENT; }
`
}

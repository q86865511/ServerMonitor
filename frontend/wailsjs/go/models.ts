export namespace main {
	
	export class AlertSettingsDTO {
	    webhook_configured: boolean;
	    cpu_percent: number;
	    memory_percent: number;
	
	    static createFrom(source: any = {}) {
	        return new AlertSettingsDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.webhook_configured = source["webhook_configured"];
	        this.cpu_percent = source["cpu_percent"];
	        this.memory_percent = source["memory_percent"];
	    }
	}
	export class AlertSettingsRequest {
	    update_webhook: boolean;
	    webhook_url: string;
	    cpu_percent: number;
	    memory_percent: number;
	
	    static createFrom(source: any = {}) {
	        return new AlertSettingsRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.update_webhook = source["update_webhook"];
	        this.webhook_url = source["webhook_url"];
	        this.cpu_percent = source["cpu_percent"];
	        this.memory_percent = source["memory_percent"];
	    }
	}
	export class CommandActionDTO {
	    action_id: string;
	    method: string;
	    path: string;
	
	    static createFrom(source: any = {}) {
	        return new CommandActionDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.action_id = source["action_id"];
	        this.method = source["method"];
	        this.path = source["path"];
	    }
	}
	export class CommandCapabilityDTO {
	    kind: string;
	    reason: string;
	    actions: CommandActionDTO[];
	
	    static createFrom(source: any = {}) {
	        return new CommandCapabilityDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.reason = source["reason"];
	        this.actions = this.convertValues(source["actions"], CommandActionDTO);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ModpackRequest {
	    type: string;
	    ref: string;
	
	    static createFrom(source: any = {}) {
	        return new ModpackRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.ref = source["ref"];
	    }
	}
	export class CreateInstanceRequest {
	    template_id: string;
	    variant: string;
	    name: string;
	    params: Record<string, string>;
	    secrets: Record<string, string>;
	    node: string;
	    modpack?: ModpackRequest;
	    runtime: string;
	    memory_mb: number;
	    cpu_percent: number;
	
	    static createFrom(source: any = {}) {
	        return new CreateInstanceRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.template_id = source["template_id"];
	        this.variant = source["variant"];
	        this.name = source["name"];
	        this.params = source["params"];
	        this.secrets = source["secrets"];
	        this.node = source["node"];
	        this.modpack = this.convertValues(source["modpack"], ModpackRequest);
	        this.runtime = source["runtime"];
	        this.memory_mb = source["memory_mb"];
	        this.cpu_percent = source["cpu_percent"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class EventDTO {
	    code: string;
	    ts_utc: string;
	    severity: string;
	    instance_uuid: string;
	    node: string;
	    template_id: string;
	    details: string;
	
	    static createFrom(source: any = {}) {
	        return new EventDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.code = source["code"];
	        this.ts_utc = source["ts_utc"];
	        this.severity = source["severity"];
	        this.instance_uuid = source["instance_uuid"];
	        this.node = source["node"];
	        this.template_id = source["template_id"];
	        this.details = source["details"];
	    }
	}
	export class InstancePortDTO {
	    name: string;
	    bind_ip: string;
	    protocol: string;
	    host_port: number;
	
	    static createFrom(source: any = {}) {
	        return new InstancePortDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.bind_ip = source["bind_ip"];
	        this.protocol = source["protocol"];
	        this.host_port = source["host_port"];
	    }
	}
	export class InstanceDTO {
	    uuid: string;
	    template_id: string;
	    variant: string;
	    name: string;
	    node: string;
	    desired_state: string;
	    observed_state: string;
	    runtime: string;
	    ports: InstancePortDTO[];
	
	    static createFrom(source: any = {}) {
	        return new InstanceDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.uuid = source["uuid"];
	        this.template_id = source["template_id"];
	        this.variant = source["variant"];
	        this.name = source["name"];
	        this.node = source["node"];
	        this.desired_state = source["desired_state"];
	        this.observed_state = source["observed_state"];
	        this.runtime = source["runtime"];
	        this.ports = this.convertValues(source["ports"], InstancePortDTO);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class MetricPointDTO {
	    ts_utc: string;
	    cpu_percent: number;
	    memory_bytes?: number;
	    memory_limit?: number;
	    player_count?: number;
	
	    static createFrom(source: any = {}) {
	        return new MetricPointDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ts_utc = source["ts_utc"];
	        this.cpu_percent = source["cpu_percent"];
	        this.memory_bytes = source["memory_bytes"];
	        this.memory_limit = source["memory_limit"];
	        this.player_count = source["player_count"];
	    }
	}
	
	export class NodeStatusDTO {
	    node: string;
	    online: boolean;
	    docker_available: boolean;
	    last_err: string;
	
	    static createFrom(source: any = {}) {
	        return new NodeStatusDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.node = source["node"];
	        this.online = source["online"];
	        this.docker_available = source["docker_available"];
	        this.last_err = source["last_err"];
	    }
	}
	export class ParamDTO {
	    key: string;
	    label: string;
	    type: string;
	    default: any;
	    required: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ParamDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.label = source["label"];
	        this.type = source["type"];
	        this.default = source["default"];
	        this.required = source["required"];
	    }
	}
	export class PortDTO {
	    name: string;
	    container: number;
	    host_port: number;
	    protocol: string;
	    required: boolean;
	
	    static createFrom(source: any = {}) {
	        return new PortDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.container = source["container"];
	        this.host_port = source["host_port"];
	        this.protocol = source["protocol"];
	        this.required = source["required"];
	    }
	}
	export class QueryEventsRequest {
	    instance_uuid: string;
	    code: string;
	    since_unix: number;
	    until_unix: number;
	    limit: number;
	
	    static createFrom(source: any = {}) {
	        return new QueryEventsRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.instance_uuid = source["instance_uuid"];
	        this.code = source["code"];
	        this.since_unix = source["since_unix"];
	        this.until_unix = source["until_unix"];
	        this.limit = source["limit"];
	    }
	}
	export class ScheduleDTO {
	    id: string;
	    instance_uuid: string;
	    kind: string;
	    at: string;
	    weekdays: number[];
	    enabled: boolean;
	    last_fired_utc: string;
	
	    static createFrom(source: any = {}) {
	        return new ScheduleDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.instance_uuid = source["instance_uuid"];
	        this.kind = source["kind"];
	        this.at = source["at"];
	        this.weekdays = source["weekdays"];
	        this.enabled = source["enabled"];
	        this.last_fired_utc = source["last_fired_utc"];
	    }
	}
	export class SecretDTO {
	    key: string;
	    label: string;
	
	    static createFrom(source: any = {}) {
	        return new SecretDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.label = source["label"];
	    }
	}
	export class SnapshotDTO {
	    uuid: string;
	    monitored: boolean;
	    has_stats: boolean;
	    stats: protocol.ResourceStats;
	    player_count?: number;
	    online?: boolean;
	    started_at: string;
	    observed_state: string;
	
	    static createFrom(source: any = {}) {
	        return new SnapshotDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.uuid = source["uuid"];
	        this.monitored = source["monitored"];
	        this.has_stats = source["has_stats"];
	        this.stats = this.convertValues(source["stats"], protocol.ResourceStats);
	        this.player_count = source["player_count"];
	        this.online = source["online"];
	        this.started_at = source["started_at"];
	        this.observed_state = source["observed_state"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class VariantDTO {
	    id: string;
	    loader: string;
	
	    static createFrom(source: any = {}) {
	        return new VariantDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.loader = source["loader"];
	    }
	}
	export class TemplateDTO {
	    id: string;
	    name: string;
	    runtime: string;
	    runtimes: string[];
	    variants: VariantDTO[];
	    params: ParamDTO[];
	    secrets: SecretDTO[];
	    ports: PortDTO[];
	    modpack: boolean;
	    has_icon: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TemplateDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.runtime = source["runtime"];
	        this.runtimes = source["runtimes"];
	        this.variants = this.convertValues(source["variants"], VariantDTO);
	        this.params = this.convertValues(source["params"], ParamDTO);
	        this.secrets = this.convertValues(source["secrets"], SecretDTO);
	        this.ports = this.convertValues(source["ports"], PortDTO);
	        this.modpack = source["modpack"];
	        this.has_icon = source["has_icon"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class UpsertScheduleRequest {
	    id: string;
	    instance_uuid: string;
	    kind: string;
	    at: string;
	    weekdays: number[];
	    enabled: boolean;
	
	    static createFrom(source: any = {}) {
	        return new UpsertScheduleRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.instance_uuid = source["instance_uuid"];
	        this.kind = source["kind"];
	        this.at = source["at"];
	        this.weekdays = source["weekdays"];
	        this.enabled = source["enabled"];
	    }
	}

}

export namespace protocol {
	
	export class BackupMeta {
	    backup_id: string;
	    instance_uuid: string;
	    game: string;
	    // Go type: time
	    ts_utc: any;
	    checksum: string;
	
	    static createFrom(source: any = {}) {
	        return new BackupMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.backup_id = source["backup_id"];
	        this.instance_uuid = source["instance_uuid"];
	        this.game = source["game"];
	        this.ts_utc = this.convertValues(source["ts_utc"], null);
	        this.checksum = source["checksum"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CommandResult {
	    success: boolean;
	    output: string;
	
	    static createFrom(source: any = {}) {
	        return new CommandResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.success = source["success"];
	        this.output = source["output"];
	    }
	}
	export class GameCommand {
	    protocol_id: string;
	    raw?: string;
	    action_id?: string;
	    args?: Record<string, string>;
	
	    static createFrom(source: any = {}) {
	        return new GameCommand(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.protocol_id = source["protocol_id"];
	        this.raw = source["raw"];
	        this.action_id = source["action_id"];
	        this.args = source["args"];
	    }
	}
	export class ResourceStats {
	    // Go type: time
	    ts_utc: any;
	    cpu_percent: number;
	    memory_bytes: number;
	    memory_limit: number;
	    data_disk_bytes?: number;
	    player_count?: number;
	    online?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ResourceStats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ts_utc = this.convertValues(source["ts_utc"], null);
	        this.cpu_percent = source["cpu_percent"];
	        this.memory_bytes = source["memory_bytes"];
	        this.memory_limit = source["memory_limit"];
	        this.data_disk_bytes = source["data_disk_bytes"];
	        this.player_count = source["player_count"];
	        this.online = source["online"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}


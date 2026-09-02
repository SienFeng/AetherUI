const RULE_ACTION = {
    PROXY: "proxy",
    BLOCK: "block",
};

const ACTION_LABEL = {
    proxy: "走节点",
    block: "阻断",
};

class DomainGroup {
    constructor(id = 0, remark = "", domains = []) {
        this.id = id;
        this.remark = remark;
        this.domains = domains;
    }

    static fromJson(json = {}) {
        let domains = [];
        try {
            domains = JSON.parse(json.domains || "[]");
        } catch (e) {
            domains = [];
        }
        return new DomainGroup(json.id, json.remark, domains);
    }

    get text() {
        return this.domains.join("\n");
    }
}

class OutboundNode {
    constructor(id = 0, tag = "", remark = "", protocol = "", config = "", enable = true) {
        this.id = id;
        this.tag = tag;
        this.remark = remark;
        this.protocol = protocol;
        this.config = config;
        this.enable = enable;
    }

    static fromJson(json = {}) {
        return new OutboundNode(json.id, json.tag, json.remark, json.protocol, json.config, json.enable);
    }
}

class RoutingRule {
    constructor(id = 0, remark = "", inboundId = 0, domainGroupId = 0,
                action = RULE_ACTION.PROXY, outboundId = 0, priority = 0, enable = true) {
        this.id = id;
        this.remark = remark;
        this.inboundId = inboundId;
        this.domainGroupId = domainGroupId;
        this.action = action;
        this.outboundId = outboundId;
        this.priority = priority;
        this.enable = enable;
    }

    static fromJson(json = {}) {
        return new RoutingRule(json.id, json.remark, json.inboundId, json.domainGroupId,
            json.action, json.outboundId, json.priority, json.enable);
    }
}

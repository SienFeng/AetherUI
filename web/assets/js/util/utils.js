class HttpUtil {
    static _handleMsg(msg) {
        if (!(msg instanceof Msg)) {
            return;
        }
        if (msg.msg === "") {
            return;
        }
        if (msg.success) {
            Vue.prototype.$message.success(msg.msg);
        } else {
            Vue.prototype.$message.error(msg.msg);
        }
    }

    static _respToMsg(resp) {
        const data = resp.data;
        if (data == null) {
            return new Msg(true);
        } else if (typeof data === 'object') {
            if (data.hasOwnProperty('success')) {
                return new Msg(data.success, data.msg, data.obj);
            } else {
                return data;
            }
        } else {
            return new Msg(false, 'unknown data:', data);
        }
    }

    static async get(url, data, options) {
        let msg;
        try {
            const resp = await axios.get(url, data, options);
            msg = this._respToMsg(resp);
        } catch (e) {
            msg = new Msg(false, e.toString());
        }
        this._handleMsg(msg);
        return msg;
    }

    static async post(url, data, options) {
        let msg;
        try {
            const resp = await axios.post(url, data, options);
            msg = this._respToMsg(resp);
        } catch (e) {
            msg = new Msg(false, e.toString());
        }
        this._handleMsg(msg);
        return msg;
    }

    static async postWithModal(url, data, modal) {
        if (modal) {
            modal.loading(true);
        }
        const msg = await this.post(url, data);
        if (modal) {
            modal.loading(false);
            if (msg instanceof Msg && msg.success) {
                modal.close();
            }
        }
        return msg;
    }
}

class PromiseUtil {

    static async sleep(timeout) {
        await new Promise(resolve => {
            setTimeout(resolve, timeout)
        });
    }

}

const seq = [
    'a', 'b', 'c', 'd', 'e', 'f', 'g',
    'h', 'i', 'j', 'k', 'l', 'm', 'n',
    'o', 'p', 'q', 'r', 's', 't',
    'u', 'v', 'w', 'x', 'y', 'z',
    '0', '1', '2', '3', '4', '5', '6', '7', '8', '9',
    'A', 'B', 'C', 'D', 'E', 'F', 'G',
    'H', 'I', 'J', 'K', 'L', 'M', 'N',
    'O', 'P', 'Q', 'R', 'S', 'T',
    'U', 'V', 'W', 'X', 'Y', 'Z'
];

class RandomUtil {

    static randomIntRange(min, max) {
        return parseInt(Math.random() * (max - min) + min, 10);
    }

    static randomInt(n) {
        return this.randomIntRange(0, n);
    }

    static randomSeq(count) {
        let str = '';
        for (let i = 0; i < count; ++i) {
            str += seq[this.randomInt(62)];
        }
        return str;
    }

    static randomLowerAndNum(count) {
        let str = '';
        for (let i = 0; i < count; ++i) {
            str += seq[this.randomInt(36)];
        }
        return str;
    }

    static randomMTSecret() {
        let str = '';
        for (let i = 0; i < 32; ++i) {
            let index = this.randomInt(16);
            if (index <= 9) {
                str += index;
            } else {
                str += seq[index - 10];
            }
        }
        return str;
    }

    static randomUUID() {
        let d = new Date().getTime();
        return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, function (c) {
            let r = (d + Math.random() * 16) % 16 | 0;
            d = Math.floor(d / 16);
            return (c === 'x' ? r : (r & 0x7 | 0x8)).toString(16);
        });
    }
}

class ObjectUtil {

    static getPropIgnoreCase(obj, prop) {
        for (const name in obj) {
            if (!obj.hasOwnProperty(name)) {
                continue;
            }
            if (name.toLowerCase() === prop.toLowerCase()) {
                return obj[name];
            }
        }
        return undefined;
    }

    static deepSearch(obj, key) {
        if (obj instanceof Array) {
            for (let i = 0; i < obj.length; ++i) {
                if (this.deepSearch(obj[i], key)) {
                    return true;
                }
            }
        } else if (obj instanceof Object) {
            for (let name in obj) {
                if (!obj.hasOwnProperty(name)) {
                    continue;
                }
                if (this.deepSearch(obj[name], key)) {
                    return true;
                }
            }
        } else {
            return obj.toString().indexOf(key) >= 0;
        }
        return false;
    }

    static isEmpty(obj) {
        return obj === null || obj === undefined || obj === '';
    }

    static isArrEmpty(arr) {
        return !this.isEmpty(arr) && arr.length === 0;
    }

    static copyArr(dest, src) {
        dest.splice(0);
        for (const item of src) {
            dest.push(item);
        }
    }

    static clone(obj) {
        let newObj;
        if (obj instanceof Array) {
            newObj = [];
            this.copyArr(newObj, obj);
        } else if (obj instanceof Object) {
            newObj = {};
            for (const key of Object.keys(obj)) {
                newObj[key] = obj[key];
            }
        } else {
            newObj = obj;
        }
        return newObj;
    }

    static deepClone(obj) {
        let newObj;
        if (obj instanceof Array) {
            newObj = [];
            for (const item of obj) {
                newObj.push(this.deepClone(item));
            }
        } else if (obj instanceof Object) {
            newObj = {};
            for (const key of Object.keys(obj)) {
                newObj[key] = this.deepClone(obj[key]);
            }
        } else {
            newObj = obj;
        }
        return newObj;
    }

    static cloneProps(dest, src, ...ignoreProps) {
        if (dest == null || src == null) {
            return;
        }
        const ignoreEmpty = this.isArrEmpty(ignoreProps);
        for (const key of Object.keys(src)) {
            if (!src.hasOwnProperty(key)) {
                continue;
            } else if (!dest.hasOwnProperty(key)) {
                continue;
            } else if (src[key] === undefined) {
                continue;
            }
            if (ignoreEmpty) {
                dest[key] = src[key];
            } else {
                let ignore = false;
                for (let i = 0; i < ignoreProps.length; ++i) {
                    if (key === ignoreProps[i]) {
                        ignore = true;
                        break;
                    }
                }
                if (!ignore) {
                    dest[key] = src[key];
                }
            }
        }
    }

    static delProps(obj, ...props) {
        for (const prop of props) {
            if (prop in obj) {
                delete obj[prop];
            }
        }
    }

    static execute(func, ...args) {
        if (!this.isEmpty(func) && typeof func === 'function') {
            func(...args);
        }
    }

    static orDefault(obj, defaultValue) {
        if (obj == null) {
            return defaultValue;
        }
        return obj;
    }

    static equals(a, b) {
        for (const key in a) {
            if (!a.hasOwnProperty(key)) {
                continue;
            }
            if (!b.hasOwnProperty(key)) {
                return false;
            } else if (a[key] !== b[key]) {
                return false;
            }
        }
        return true;
    }

}

// 出站节点的身份色：系统状态页的节点头像、分流页的节点头像与规则去向胶囊、
// 访问日志的走向标签共用这一张表，同一个节点在任何地方都是同一个颜色。
// 按节点 id 取色而不是按 tag 哈希：id 是连续分配的，相邻节点必然不同色；
// 哈希在只有两三个节点时也可能撞色，而那正是最常见的部署规模。
// solid 是头像底色（白字 ≥ 5.3:1），text/bg 是胶囊的文字与底色（≥ 5.1:1）。
// 刻意不含红/粉/绿/橙：去向这一列里红是封禁、绿是直连、橙是链式代理，
// 粉色胶囊挨着红色「封禁」时一眼分不开。
const NODE_TONES = [
    { solid: '#1266D6', text: '#1D4ED8', bg: '#EFF4FF' },   // 蓝
    { solid: '#7C3AED', text: '#6D28D9', bg: '#F5F3FF' },   // 紫
    { solid: '#0E7490', text: '#0E7490', bg: '#ECFEFF' },   // 青
    { solid: '#4F46E5', text: '#4338CA', bg: '#EEF2FF' },   // 靛
];

class NodeTone {
    static of(id) {
        const n = Number(id) || 0;
        return NODE_TONES[((n % NODE_TONES.length) + NODE_TONES.length) % NODE_TONES.length];
    }

    // 去向胶囊 / 走向标签的内联样式（圆点用 currentColor，跟着文字色走）
    static pillStyle(id) {
        const t = NodeTone.of(id);
        return { color: t.text, background: t.bg };
    }
}

// 共享风险徽标的文案 / 配色 / 图标。入站列表与系统状态页的入站用量排行共用，
// 两边各写一份迟早会分叉，而分叉后同一个入站在两页显示两种结论，不会有任何提示。
// r 是 /aui/inbound/sharing/risk/summary 里某个入站的那一项，没有时按「学习中」：
// 入站刚建好、或引擎版本变了都会这样，把它显示成「低风险」等于报一个没算过的结论。
class SharingRisk {
    static of(r) {
        return r || { state: 'learning', level: '', score: 0 };
    }

    static label(r) {
        r = SharingRisk.of(r);
        if (r.state === 'unobservable') return '不可评估';
        if (r.state === 'degraded') return '降级';
        if (r.state !== 'ready') return '学习中';
        return '风险 ' + (SharingRisk.LEVEL_TEXT[r.level] || r.level);
    }

    // 非 ready 一律中性：把「测不了」「还没测够」渲染成绿色或红色都是在报一个没算过的结论。
    static tone(r) {
        r = SharingRisk.of(r);
        if (r.state !== 'ready') return 'status-neutral';
        return SharingRisk.LEVEL_TONE[r.level] || 'status-neutral';
    }

    static icon(r) {
        r = SharingRisk.of(r);
        if (r.state !== 'ready') return 'question-circle';
        return SharingRisk.LEVEL_ICON[r.level] || '';
    }
}

SharingRisk.LEVEL_TEXT = { low: '低', watch: '观察', medium: '中', high: '高', severe: '严重' };
// 风险徽标用 custom.css 的 .status-badge 一套 class，不走 a-tag（见 CLAUDE.md「已知偏差」）。
SharingRisk.LEVEL_TONE = {
    low:    'status-ok',
    watch:  'status-warn',
    medium: 'status-warn',
    high:   'status-danger',
    severe: 'status-danger',
};
SharingRisk.LEVEL_ICON = {
    low:    'check-circle',
    watch:  'exclamation-circle',
    medium: 'exclamation-circle',
    high:   'warning',
    severe: 'warning',
};

// 出口国家的显示名：「美国 (US)」；浏览器不支持 Intl.DisplayNames 时只给代码。
// 分流管理与系统状态两页的出站节点国旗共用。
function countryName(code) {
    try {
        const name = new Intl.DisplayNames(['zh-CN'], { type: 'region' }).of(code);
        return name && name !== code ? name + ' (' + code + ')' : code;
    } catch (e) {
        return code;
    }
}

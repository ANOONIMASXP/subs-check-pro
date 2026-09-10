// sub_store\init.js

// 接收 Go 层注入的常量字面量
const $loon = __LOON_JSON_OBJ__;
const $script = __SCRIPT_JSON_OBJ__;
var $request = {};
var $argument = "";

const $persistentStore = {
    read: function (key) {
        let val = __ps_read(key);
        return val === undefined ? null : val;
    },
    write: function (val, key) {
        __ps_write(val, key);
        return true;
    }
};

const $notification = {
    post: function (title, subtitle, content) {
        __notify_post(title, subtitle, content);
    }
};

// 智能日志防洪堤，阻断重复 Error/Warn 跨越 CGO 疯狂损耗性能
const __log_cache = {};
function __throttled_log(level, ...args) {
    let msg = args.map(String).join(" ");
    let key = level + ":" + msg;
    __log_cache[key] = (__log_cache[key] || 0) + 1;

    if (__log_cache[key] <= 3) {
        __console_log(level, msg);
    } else if (__log_cache[key] === 4) {
        __console_log(level, msg + " (⚠️ 该日志重复出现，已折叠隐藏...)");
    }
}

const console = {
    log: function (...args) { __throttled_log("log", ...args); },
    info: function (...args) { __throttled_log("info", ...args); },
    warn: function (...args) { __throttled_log("warn", ...args); },
    error: function (...args) { __throttled_log("error", ...args); }
};

const $httpClient = {};
const __http_callbacks = {};
let __http_req_id = 0;

['get', 'post', 'put', 'patch', 'delete', 'head', 'options'].forEach(method => {
    $httpClient[method] = function (options, callback) {
        let req = typeof options === 'string' ? { url: options } : options;
        let reqId = "req_" + (++__http_req_id);
        if (callback) { __http_callbacks[reqId] = callback; }
        __go_http_request_async(method.toUpperCase(), JSON.stringify(req), reqId);
    };
});

function __dispatch_http_response(reqId, resMetaJson) {
    let cb = __http_callbacks[reqId];
    if (!cb) return;
    delete __http_callbacks[reqId];
    let res = JSON.parse(resMetaJson);
    let err = res.error !== undefined ? res.error : null;
    let resp = res.response !== undefined ? res.response : null;
    if (resp && res.reqId !== undefined) {
        resp.body = __go_http_request_body(res.reqId);
    }
    cb(err, resp, resp ? resp.body : null);
}

// 终极优化：在 JS 侧剥离巨型 Body 绕过 JSON 序列化
const $done = function (val) {
    let bodyStr = undefined;
    if (val && typeof val === 'object') {
        if (val.response && val.response.body !== undefined) {
            bodyStr = typeof val.response.body === 'string' ? val.response.body : JSON.stringify(val.response.body);
            delete val.response.body;
        } else if (val.body !== undefined) {
            bodyStr = typeof val.body === 'string' ? val.body : JSON.stringify(val.body);
            delete val.body;
        }
    }
    let metaJson = val ? JSON.stringify(val) : "{}";
    __done(metaJson, bodyStr || "");
};

// 隔离执行主脚本
function __run_sub_store_script() {
    __SCRIPT_SRC__
}
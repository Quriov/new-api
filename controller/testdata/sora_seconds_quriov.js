export const meta = {
  apiVersion: 1,
  key: "sora",
  name: "Sora",
  icon: "Sora.Color",
  description: {
    en: "OpenAI Sora video generation (text-to-video, image-to-video, and remix)",
    zh: "OpenAI Sora 视频生成（文生视频、图生视频、remix）",
  },
  // Quriov：在出厂 sora 1.1.0 上只「新增」quriov_* 用量（见文件末尾 quriovTierFacts），
  // 出厂已有的 seconds / size 一个字没改。
  version: "1.1.0-quriov.8",
  channelTypes: [55, 1], // OpenAI-type channels natively serve sora with the same wire format
  author: { name: "QuantumNous" },
  models: ["sora-2", "sora-2-pro", "wan3.0-video", "wan3.0-video-prime", "wan2.6-t2v", "wan2.6-i2v", "doubao-seedance-2-0-260128", "MiniMax-H3"],
  fetchMode: "per_task",
  // A New API gateway serves /v1/videos as a host protocol, so no URL changes.
  upstreams: ["vendor", "new_api"],
  usageSchema: {
    // Requested video duration in seconds.
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
    // Requested output video dimensions.
    size: {
      enum: ["720x1280", "1280x720", "1792x1024", "1024x1792"],
      enumLabels: {
        "720x1280": { en: "720x1280", zh: "720x1280" },
        "1280x720": { en: "1280x720", zh: "1280x720" },
        "1792x1024": { en: "1792x1024", zh: "1792x1024" },
        "1024x1792": { en: "1024x1792", zh: "1024x1792" },
      },
      description: { en: "Output video dimensions", zh: "输出视频尺寸" },
    },
    // ---- Quriov 新增（只对 QURIOV_TIERED_MODELS 里的型号报；别的型号一个都不报）----
    // 名字带 quriov_ 前缀：请求体里不会有同名字段，宿主按 schema 校验请求体时碰不到它们。
    quriov_seconds: {
      type: "number",
      unit: "second",
      description: { en: "Billed seconds (upstream model rule)", zh: "计费秒数（按上游型号规则）" },
    },
    quriov_resolution: {
      enum: ["480p", "540p", "720p", "768p", "1080p", "2k", "4k", "unknown"],
      description: { en: "Output resolution tier", zh: "输出分辨率档" },
    },
    quriov_audio: {
      enum: ["on", "off", "unset"],
      description: { en: "Audio generation", zh: "是否生成音频" },
    },
    quriov_reference: {
      enum: ["none", "yes"],
      description: { en: "Has image / video reference input", zh: "是否带参考图 / 参考视频 / 首尾帧" },
    },
    quriov_offpeak: {
      enum: ["off", "on"],
      description: { en: "Off-peak mode", zh: "闲时模式" },
    },
    quriov_scene: {
      enum: ["none", "motion_control", "template_effect", "other"],
      description: { en: "Special scene", zh: "特殊场景" },
    },
  },
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};


// 六个共享视频型号：size 是上游分辨率档，不受 Sora 像素尺寸枚举约束。
// 保留 seconds 和全部 quriov_* 校验；既有型号继续使用原始 schema。
const { size: quriovSoraSizeSchema, ...quriovSharedVideoSchema } = meta.usageSchema;
meta.usageProfiles = [{
  models: ["wan3.0-video", "wan3.0-video-prime", "wan2.6-t2v", "wan2.6-i2v", "doubao-seedance-2-0-260128", "MiniMax-H3"],
  schema: quriovSharedVideoSchema,
}];

function trimmed(value) {
  return String(value || "").trim();
}

function responsesInput(req) {
  const texts = [],
    images = [];
  const input = req.input;
  if (typeof input === "string") texts.push(input);
  else if (Array.isArray(input)) {
    for (const item of input) {
      if (typeof item === "string") {
        texts.push(item);
        continue;
      }
      if (!item || typeof item !== "object" || Array.isArray(item)) continue;
      const content = item.content === undefined ? [item] : Array.isArray(item.content) ? item.content : [item.content];
      for (const part of content) {
        if (typeof part === "string") {
          texts.push(part);
          continue;
        }
        if (!part || typeof part !== "object" || Array.isArray(part)) continue;
        if (["input_text", "text"].includes(part.type) && typeof part.text === "string") texts.push(part.text);
        if (["input_image", "image_url"].includes(part.type)) {
          let image = part.image_url;
          if (image && typeof image === "object") image = image.url;
          if (trimmed(image)) images.push(trimmed(image));
        }
      }
    }
  }
  return {
    prompt: texts
      .filter(function (text) {
        return trimmed(text);
      })
      .join("\n"),
    images: images,
  };
}

function responsesVideoText(ctx) {
  const artifact = ctx && ctx.artifacts && ctx.artifacts.video;
  const url = trimmed(artifact && artifact.url);
  if (!url) throw new Error("video artifact is unavailable");
  const escaped = url.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return '<video controls src="' + escaped + '"></video>';
}

function requestValues(req, model) {
  const values = Object.assign({}, req || {});
  values.model = model;
  return values;
}

function quriovGeeknow(baseUrl) {
  const match = /^https?:\/\/([^/?#]+)/i.exec(String(baseUrl || ""));
  if (!match) return false;
  const authority = match[1].split("@").pop();
  return authority.replace(/:[0-9]+$/, "").toLowerCase() === "www.geeknow.top";
}

function quriovJsonValues(ctx, req) {
  const values = requestValues(req, ctx.upstreamModel);
  if (quriovGeeknow(ctx.baseUrl) && typeof values.seconds === "number") values.seconds = String(values.seconds);
  return values;
}

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody || {};
  const h3 = ctx.model === "MiniMax-H3" && quriovGeeknow(ctx.baseUrl);
  const h3Text = h3 && Array.isArray(req.content) && req.content.some(function (part) { return part && part.type === "text" && String(part.text || "").trim(); });
  if (!String(req.prompt || "").trim() && !h3Text) throw new Error("field prompt is required");
  // H3 网关只按显式 duration 生成；不将秒数字段隐式改成长视频。
  if (h3 && (typeof req.duration !== "number" || !Number.isInteger(req.duration) || req.duration < 1 || req.duration > 15)) throw new Error("MiniMax-H3 requires duration (integer 1-15)");
  quriovRejectMultiImage(ctx.model, req);
  const action = ctx.action === "remix" ? "remix" : ctx.action;
  const headers = { Authorization: "Bearer " + ctx.apiKey };
  if (action === "remix") {
    headers["Content-Type"] = "application/json";
    return { url: ctx.baseUrl + "/v1/videos/" + ctx.originTaskId + "/remix", method: "POST", headers, body: quriovJsonValues(ctx, req), action };
  }
  if ((ctx.files || []).length) {
    const parts = [];
    const values = requestValues(req, ctx.upstreamModel);
    for (const key of Object.keys(values)) {
      if (values[key] !== undefined && values[key] !== null && typeof values[key] !== "object") parts.push({ name: key, value: values[key] });
    }
    if (values.metadata && typeof values.metadata === "object" && !Array.isArray(values.metadata)) {
      parts.push({ name: "metadata", value: JSON.stringify(values.metadata) });
    }
    for (const file of ctx.files) parts.push({ name: file.field, fileRef: file.ref, filename: file.filename });
    return { url: ctx.baseUrl + "/v1/videos", method: "POST", headers, bodyType: "multipart", parts };
  }
  headers["Content-Type"] = "application/json";
  return { url: ctx.baseUrl + "/v1/videos", method: "POST", headers, body: quriovJsonValues(ctx, req) };
}

export function parseSubmitResponse(ctx, resp) {
  const body = resp.body || {};
  const taskId = body.id || body.task_id;
  if (!taskId) throw new Error("task_id is empty");
  return { taskId, taskData: body };
}

export function extractUsage(ctx) {
  if (ctx.action === "remix") return {};
  const req = ctx.requestBody || {};
  let seconds = Number(req.seconds || req.duration || 4);
  if (!Number.isFinite(seconds) || seconds <= 0) seconds = 4;
  const facts = { seconds: Math.min(seconds, 3600), size: req.size || "720x1280" };
  // Quriov：只对名单里的型号追加 quriov_* 用量；不在名单里的型号走到这里与出厂 1.1.0 逐字节相同。
  const extra = quriovTierFacts(ctx);
  if (extra) Object.assign(facts, extra);
  return facts;
}

// ===========================================================================
// Quriov 新增：按上游（geeknow）价目表分档要用的请求参数
// 依据：https://docs.geeknow.ai/api-reference/videos/aigc/overview.md（国产视频模型：时长 / 分辨率优先级、
//      metadata.output_config.audio_generation、metadata.offpeak、metadata.scene_type、参考输入字段）
//      https://docs.geeknow.ai/api-reference/videos/wan3/generation.md（Wan 3.0：size 是分辨率档，缺省 1080P）
// 原则：读不准的一律报 unknown / unset / yes，让计费表达式按「可能的最贵那档」收，宁可多收不倒挂。
// ===========================================================================
// 型号 → 上游文档写明的缺省分辨率（文档没写的报 unknown）
const QURIOV_TIERED_MODELS = {
  "wan2.6-t2v": { family: "relist", defaultRes: "1080p", defaultSeconds: 5 },
  "wan2.6-i2v": { family: "relist", defaultRes: "1080p", defaultSeconds: 5 },
  "MiniMax-H3": { family: "relist", defaultRes: "768p", defaultSeconds: 15 },
  "Hailuo-02": { family: "aigc", defaultRes: "768p", defaultSeconds: 6 },
  "Hailuo-2.3": { family: "aigc", defaultRes: "768p", defaultSeconds: 6 },
  "Hailuo-2.3-fast": { family: "aigc", defaultRes: "768p", defaultSeconds: 6 },
  "Hunyuan-1.5": { family: "aigc", defaultRes: "unknown" },
  "Kling-1.6": { family: "aigc", defaultRes: "720p" },
  "Kling-2.0": { family: "aigc", defaultRes: "720p" },
  "Kling-2.1": { family: "aigc", defaultRes: "720p" },
  "Kling-2.5": { family: "aigc", defaultRes: "720p" },
  "Kling-2.6": { family: "aigc", defaultRes: "720p" },
  "Kling-3.0": { family: "aigc", defaultRes: "720p" },
  "Kling-3.0-Omni": { family: "aigc", defaultRes: "720p" },
  "Kling-O1": { family: "aigc", defaultRes: "720p" },
  "Mingmou-1.0": { family: "aigc", defaultRes: "unknown" },
  "SV-1.0-lite": { family: "aigc", defaultRes: "unknown" },
  "SV-1.0-pro": { family: "aigc", defaultRes: "unknown" },
  "SV-1.0-pro-fast": { family: "aigc", defaultRes: "unknown" },
  "SV-1.5-pro": { family: "aigc", defaultRes: "unknown" },
  "Vidu-q2": { family: "aigc", defaultRes: "720p" },
  "Vidu-q2-pro": { family: "aigc", defaultRes: "720p" },
  "Vidu-q2-turbo": { family: "aigc", defaultRes: "720p" },
  "Vidu-q3-pro": { family: "aigc", defaultRes: "720p" },
  "Vidu-q3-turbo": { family: "aigc", defaultRes: "720p" },
  "wan3.0-video": { family: "wan3", defaultRes: "1080p" },
  "wan3.0-video-prime": { family: "wan3", defaultRes: "1080p" },
};

// 分辨率档的「短边像素」，用来把任意写法向上取到档
const QURIOV_RES_LADDER = [
  ["480p", 480],
  ["540p", 540],
  ["720p", 720],
  ["768p", 768],
  ["1080p", 1080],
  ["2k", 1440],
  ["4k", 2160],
];

function quriovObject(value) {
  return value && typeof value === "object" && !Array.isArray(value) ? value : {};
}

function quriovPresent(value) {
  return value !== undefined && value !== null && String(value).trim() !== "";
}

function quriovResolution(value) {
  const text = String(value).trim().toLowerCase();
  let pixels = 0;
  let match = /^(\d+)\s*p$/.exec(text);
  if (match) pixels = Number(match[1]);
  else if (text === "2k") pixels = 1440;
  else if (text === "4k") pixels = 2160;
  else {
    match = /^(\d+)\s*[x*×]\s*(\d+)$/.exec(text);
    if (match) pixels = Math.min(Number(match[1]), Number(match[2]));
  }
  if (!(pixels > 0)) return "unknown";
  for (const [name, limit] of QURIOV_RES_LADDER) if (pixels <= limit) return name;
  return "unknown";
}

function quriovRank(name) {
  for (let i = 0; i < QURIOV_RES_LADDER.length; i++) if (QURIOV_RES_LADDER[i][0] === name) return i;
  return QURIOV_RES_LADDER.length;
}

function quriovSeconds(candidates, defaultSeconds = 5) {
  for (const value of candidates) {
    if (!quriovPresent(value)) continue;
    const n = Number(value);
    if (Number.isFinite(n) && n > 0) return Math.min(Math.ceil(n), 3600);
  }
  return defaultSeconds;
}

function quriovWan26Resolution(value) {
  const text = String(value).trim().toLowerCase().replace(/[x*×]/g, "x");
  if (["720p", "1280x720", "720x1280", "960x960", "1088x832", "832x1088"].includes(text)) return "720p";
  if (["1080p", "1920x1080", "1080x1920", "1440x1440", "1632x1248", "1248x1632"].includes(text)) return "1080p";
  return "unknown";
}

function quriovTierFacts(ctx) {
  try {
    const model = typeof ctx.model === "string" ? ctx.model : "";
    const spec = Object.prototype.hasOwnProperty.call(QURIOV_TIERED_MODELS, model) ? QURIOV_TIERED_MODELS[model] : null;
    if (!spec) return null;
    const req = quriovObject(ctx.requestBody);
    const meta = quriovObject(req.metadata);
    const output = quriovObject(meta.output_config);
    const params = quriovObject(meta.parameters);
    const facts = {};

    let resolution;
    if (spec.family === "relist") {
      let native = req.parameters;
      if (typeof native === "string") {
        try { native = JSON.parse(native); } catch (e) { native = null; }
      }
      native = quriovObject(native);
      const explicit = [req.seconds, req.duration, meta.seconds, meta.duration, meta.video_duration,
        output.duration, output.Duration, params.duration, native.duration].filter(quriovPresent);
      facts.quriov_seconds = explicit.length ? Math.max(...explicit.map(function (v) { return quriovSeconds([v], spec.defaultSeconds); })) : spec.defaultSeconds;
      const sizes = [req.size, req.resolution, meta.resolution, meta.size, output.resolution, output.Resolution,
        params.resolution, params.size, native.resolution, native.size].filter(quriovPresent);
      const found = sizes.map(model === "MiniMax-H3" ? quriovResolution : quriovWan26Resolution);
      resolution = found.length ? found.sort(function (a, b) { return quriovRank(b) - quriovRank(a); })[0] : spec.defaultRes;
    } else if (spec.family === "wan3") {
      // Wan 3.0：原有优先级与原生 parameters 均显式给出时取大；未知分辨率按最高档。
      let native = req.parameters;
      if (typeof native === "string") {
        try { native = JSON.parse(native); } catch (e) { native = null; }
      }
      native = quriovObject(native);
      const seconds = [req.seconds, req.duration, params.duration].filter(quriovPresent);
      const durations = [];
      if (seconds.length) durations.push(quriovSeconds(seconds));
      if (quriovPresent(native.duration)) durations.push(quriovSeconds([native.duration]));
      facts.quriov_seconds = durations.length ? Math.max(...durations) : 5;
      const found = [];
      if (quriovPresent(req.size)) found.push(/x|\*|×/i.test(String(req.size)) ? "unknown" : quriovResolution(req.size));
      else if (quriovPresent(params.resolution)) found.push(quriovResolution(params.resolution));
      if (quriovPresent(native.resolution)) found.push(quriovResolution(native.resolution));
      resolution = found.length ? found.sort(function (a, b) { return quriovRank(b) - quriovRank(a); })[0] : spec.defaultRes;
    } else {
      // 国产视频模型：seconds > duration > metadata.seconds / duration / video_duration > 5
      const explicit = [req.seconds, req.duration, meta.seconds, meta.duration, meta.video_duration].filter(quriovPresent);
      const durations = [];
      if (explicit.length) durations.push(quriovSeconds(explicit));
      for (const value of [output.duration, output.Duration]) if (quriovPresent(value)) durations.push(quriovSeconds([value]));
      facts.quriov_seconds = durations.length ? Math.max(...durations) : quriovSeconds([], spec.defaultSeconds || 5);
      if (String(meta.scene_type).trim().toLowerCase() === "lip_sync") facts.quriov_seconds = Math.max(5, facts.quriov_seconds);
      // metadata.output_config.resolution > 顶层 size > 型号缺省；两处都写了按高的那档
      const found = [output.resolution, output.Resolution, req.size].filter(quriovPresent).map(quriovResolution);
      if (!found.length) resolution = spec.defaultRes;
      else if (found.includes("unknown")) resolution = "unknown";
      else resolution = found.sort(function (a, b) { return quriovRank(b) - quriovRank(a); })[0];
    }
    if (spec.family === "aigc" && String(meta.scene_type).trim().toLowerCase() === "motion_control" && String(meta.motion_level).trim().toLowerCase() === "pro" && quriovRank(resolution) < quriovRank("1080p")) resolution = "1080p";
    facts.quriov_resolution = resolution;

    const audio = spec.family === "aigc" && !quriovPresent(output.audio_generation) ? output.AudioGeneration : output.audio_generation;
    const audioText = String(audio === undefined || audio === null ? "" : audio).trim().toLowerCase();
    if (audio === true || audioText === "enabled" || audioText === "true") facts.quriov_audio = "on";
    else if (audio === false || audioText === "disabled" || audioText === "false") facts.quriov_audio = "off";
    else facts.quriov_audio = "unset";

    const images = Array.isArray(req.images) ? req.images.length > 0 : quriovPresent(req.images);
    // 注意：不看 ctx.action —— 旧式路由下宿主一律把 action 记成 image_to_video（离线试算实测），看它会把纯文生视频也算成带参考。
    let reference = images || quriovPresent(req.image) || quriovPresent(req.input_reference) || (ctx.files || []).length > 0;
    for (const key of Object.keys(req)) if (key.indexOf("last_frame") === 0 && quriovPresent(req[key])) reference = true;
    for (const key of ["video_url", "file_infos", "image", "images", "image_url", "reference_images", "reference_video", "first_frame", "last_frame"]) {
      const value = meta[key];
      if (Array.isArray(value) ? value.length > 0 : quriovPresent(value) && typeof value !== "object") reference = true;
      if (value && typeof value === "object" && !Array.isArray(value) && Object.keys(value).length) reference = true;
    }
    facts.quriov_reference = reference ? "yes" : "none";

    facts.quriov_offpeak = meta.offpeak === true || String(meta.offpeak).trim().toLowerCase() === "true" ? "on" : "off";

    const scene = String(meta.scene_type === undefined || meta.scene_type === null ? "" : meta.scene_type).trim().toLowerCase();
    facts.quriov_scene = !scene ? "none" : scene === "motion_control" || scene === "template_effect" ? scene : "other";
    return facts;
  } catch (e) {
    // 读参数出任何意外：不报 quriov_*（出厂那两项照旧）。表达式读不到 quriov_seconds 会算错 → 宿主整单 400，
    // 也就是宁可拒单、不按错的档收钱。
    return null;
  }
}

export function extractUsageOnComplete(task, taskResult, body) {
  const facts = {};
  const seconds = Number((body || {}).seconds || (body || {}).duration || 0);
  if (Number.isFinite(seconds) && seconds > 0) facts.seconds = Math.min(seconds, 3600);
  const size = trimmed((body || {}).size);
  if (["720x1280", "1280x720", "1792x1024", "1024x1792"].includes(size)) facts.size = size;
  return facts;
}

export function buildQueryRequest(ctx) {
  return { url: ctx.baseUrl + "/v1/videos/" + ctx.taskId, method: "GET", headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export function parseTaskResult(ctx, body) {
  const statuses = {
    queued: "QUEUED",
    pending: "QUEUED",
    processing: "IN_PROGRESS",
    in_progress: "IN_PROGRESS",
    completed: "SUCCESS",
    failed: "FAILURE",
    cancelled: "FAILURE",
  };
  const mapped = statuses[body.status];
  const result = { status: mapped || "UNKNOWN" };
  if (!mapped) result.reason = "unrecognized status: " + String(body.status || "");
  if (body.progress > 0 && body.progress < 100) result.progress = body.progress + "%";
  if (result.status === "FAILURE") result.reason = body.error && body.error.message ? body.error.message : "task failed";
  return result;
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return {
    url: ctx.baseUrl + "/v1/videos/" + encodeURIComponent(ctx.upstreamTaskId) + "/content",
    method: ctx.clientRequest.method,
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export const protocols = {
  openai_responses: {
    decodeRequest: function (ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
      const req = ctx.body.value;
      if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be an object");
      const model = trimmed(req.model);
      if (!model) throw new Error("model is required");
      if (req.input !== undefined && typeof req.input !== "string" && !Array.isArray(req.input)) throw new Error("input must be a string or array");
      if (req.images !== undefined && !Array.isArray(req.images)) throw new Error("images must be an array");
      if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata)))
        throw new Error("metadata must be an object");
      const input = responsesInput(req);
      const prompt = input.prompt || trimmed(req.prompt);
      if (!prompt) throw new Error("input is required");
      const images = [];
      for (const image of [req.image, req.input_reference].concat(req.images || [], input.images)) {
        if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
      }
      const requestBody = { model: model, prompt: prompt };
      if (images.length) requestBody.input_reference = images[0];
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.seconds = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.seconds = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      if (Object.prototype.hasOwnProperty.call(req, "metadata")) requestBody.metadata = req.metadata;
      return { kind: "submit", model: model, action: images.length ? "image_to_video" : "text_to_video", requestBody: requestBody };
    },
    renderEvents: function (ctx, task, previousState) {
      const status = String(task.status || "UNKNOWN").toUpperCase();
      const value = Number(String(task.progress || "").replace("%", ""));
      const progress = Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
      const state = { status: status, progress: progress };
      if (status === "SUCCESS") {
        const text = responsesVideoText(ctx);
        const events = previousState && previousState.status === status ? [] : [{ type: "output", data: text }];
        return { events: events, state: state, done: true };
      }
      if (status === "FAILURE")
        return { events: [{ type: "error", code: "task_failed", message: task.fail_reason || "task failed" }], state: state, done: true };
      if (previousState && previousState.status === status && previousState.progress === progress) return { events: [], state: state, done: false };
      const event = { type: "progress", message: status.toLowerCase() };
      if (progress !== null) event.progress = progress;
      return { events: [event], state: state, done: false };
    },
    renderFinal: function (ctx, _task) {
      return {
        output: [
          {
            type: "message",
            status: "completed",
            role: "assistant",
            content: [{ type: "output_text", text: responsesVideoText(ctx), annotations: [], logprobs: [] }],
          },
        ],
        metadata: { vendor: "sora" },
      };
    },
  },
};

const legacyRenderers = {
  openai_video: function (task) {
    const statuses = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
    const output = {
      id: task.task_id,
      object: "video",
      model: (task.properties || {}).origin_model_name || "",
      status: statuses[task.status] || "unknown",
      progress: Number(String(task.progress || "0").replace("%", "")),
      created_at: Number(task.created_at || 0),
    };
    const completedAt = Number(task.finished_at || task.updated_at || 0);
    if (completedAt > 0) output.completed_at = completedAt;
    if (task.status === "FAILURE") {
      output.error = { code: "video_generation_failed", message: "The video generation task failed." };
    }
    return output;
  },
};

protocols.openai_video = {
  decodeRequest: function (ctx) {
    if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
    if (ctx.body.kind === "json") {
      if (!ctx.body.value || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
      const req = ctx.body.value;
      const seconds = req.seconds === undefined ? req.duration : req.seconds;
      if (seconds !== undefined && (!Number.isFinite(Number(seconds)) || Number(seconds) <= 0 || Number(seconds) > 3600))
        throw new Error("seconds must be between 1 and 3600");
      return {
        kind: "submit",
        model: ctx.model,
        action: req.input_reference || req.image ? "image_to_video" : "text_to_video",
        requestBody: Object.assign({}, req, { model: ctx.model }),
      };
    }
    const first = function (name) {
      const values = (ctx.body.fields || {})[name] || [];
      if (values.length > 1) throw new Error(name + " must be provided once");
      return values[0];
    };
    const req = {};
    const fields = ctx.body.fields || {};
    for (const name of Object.keys(fields)) {
      req[name] = first(name);
    }
    let hasInputReferenceFile = false;
    for (const file of ctx.body.files || []) {
      if (file.field !== "input_reference") throw new Error("unexpected file field: " + file.field);
      if (hasInputReferenceFile) throw new Error("input_reference must be provided once");
      hasInputReferenceFile = true;
    }
    if (req.metadata !== undefined) {
      let parsed;
      try {
        parsed = JSON.parse(req.metadata);
      } catch (e) {
        throw new Error("metadata must be a JSON object string");
      }
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("metadata must be a JSON object string");
      req.metadata = parsed;
    }
    if (req.seconds !== undefined) req.seconds = Number(req.seconds);
    else if (req.duration !== undefined) req.seconds = Number(req.duration);
    const seconds = req.seconds === undefined ? req.duration : req.seconds;
    if (seconds !== undefined && (!Number.isFinite(Number(seconds)) || Number(seconds) <= 0 || Number(seconds) > 3600))
      throw new Error("seconds must be between 1 and 3600");
    return {
      kind: "submit",
      model: ctx.model,
      action: hasInputReferenceFile || req.input_reference || req.image ? "image_to_video" : "text_to_video",
      requestBody: Object.assign({}, req, { model: ctx.model }),
    };
  },
  render: function (ctx, task) {
    if (task.data && typeof task.data === "object" && !Array.isArray(task.data)) return task.data;
    return legacyRenderers.openai_video(task);
  },
};

// ===========================================================================
// Quriov 10-29：按次计费的图片型号每单只出一张（scripts/newapi/catalog-2026-10/batch-20261029-single-image-guard）
// 这些型号在 TASK_PRICE_PATCH 里，按 ModelPrice 每单收一次、不乘张数；请求里要多张就在预扣费之前拒掉
// （宿主在 ValidateRequestAndSetAction 里调 buildSubmitRequest，早于计价与预扣），免得上游认了张数而我们只收一张。
// 只看客户请求里的原始型号名（映射前）；名单外的型号走到这里什么都不做。
// 张数 = 1、不传、0、空、读不成数字 ⇒ 按一张放行（交给上游）；能读成 > 1 的数 ⇒ 拒。
// ===========================================================================
const QURIOV_SINGLE_IMAGE_MODELS = [
  "gpt-image-2", "gpt-image-2-2K", "gpt-image-2-4K",
  "gpt-image-2.5-1K", "gpt-image-2.5-2K", "gpt-image-2.5-4K",
  "gpt-image-2.5-flare-vip", "gpt-image-2.5-sunburst-vip",
  "nano_banana_2", "nano_banana_2-2K", "nano_banana_2-4K",
  "nano_banana_pro-1K", "nano_banana_pro-2K", "nano_banana_pro-4K",
  "nano-banana-2.1",
  "gpt-image-2.6-1K",
  "gpt-image-2.6-2K",
  "gpt-image-2.6-4K",
  "gpt-image-2.6-sunburst-1K",
  "gpt-image-2.6-sunburst-2K",
  "gpt-image-2.6-sunburst-4K",
  "gpt-image-2.6-gif",
];
const QURIOV_IMAGE_COUNT_FIELDS = ["n", "num_images", "image_count", "number_of_images", "batch_size", "count"];
const QURIOV_SINGLE_IMAGE_MESSAGE =
  "该型号每次请求只出一张图，请把 n 设为 1 或不传；要多张请多次请求。" +
  "This model returns one image per request: set n to 1 or omit it, and send separate requests for more images.";

function quriovCountAboveOne(value) {
  if (typeof value !== "number" && typeof value !== "string") return false;
  if (typeof value === "string" && !value.trim()) return false;
  const count = Number(value);
  return !Number.isNaN(count) && count > 1;
}

function quriovRejectMultiImage(model, req) {
  if (QURIOV_SINGLE_IMAGE_MODELS.indexOf(model) < 0 || !req || typeof req !== "object" || Array.isArray(req)) return;
  let metadata = req.metadata;
  if (typeof metadata === "string") {
    try {
      metadata = JSON.parse(metadata);
    } catch (_err) {
      metadata = null;
    }
  }
  const sources = [req];
  if (metadata && typeof metadata === "object" && !Array.isArray(metadata)) sources.push(metadata);
  for (const source of sources) {
    for (const field of QURIOV_IMAGE_COUNT_FIELDS) {
      if (Object.prototype.hasOwnProperty.call(source, field) && quriovCountAboveOne(source[field])) throw new Error(QURIOV_SINGLE_IMAGE_MESSAGE);
    }
  }
}

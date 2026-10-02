// Async image relay: serves image models sold on the task-style /v1/videos surface from an upstream whose
// image API is "POST /v1/images/generations/async" + "GET /v1/images/generations/async/{taskId}".
//
// This file is NOT embedded in the binary (only plugins/tasks/ is). It is uploaded from the admin console and
// bound to a "Task Plugin" channel; the upstream address and key come from that channel. See README.md.

// Seconds after submission at which a still-unfinished upstream task is failed (and refunded) by this plugin.
const POLL_DEADLINE_SECONDS = 600;
const MAX_REFERENCE_IMAGES = 8;

// Long edge (px) the delivered image must reach for each tier.
const TIER_MIN_LONG_EDGE = { "1K": 1000, "2K": 2000, "4K": 2800 };

// aspect ratio -> "WxH" sent as `size`. The first six ratios of every table are the upstream's documented size
// tables; the remaining four are only offered on the "2.5" family, where they were measured against the upstream.
const BASE_SIZES = {
  "1K": { "1:1": "1024x1024", "4:3": "1536x1152", "3:2": "1536x1024", "2:3": "1024x1536", "16:9": "1920x1080", "9:16": "1080x1920" },
  "2K": { "1:1": "2048x2048", "4:3": "2048x1536", "3:2": "2560x1712", "2:3": "1712x2560", "16:9": "2048x1152", "9:16": "1152x2048" },
  "4K": { "1:1": "2880x2880", "4:3": "3840x2880", "3:2": "3840x2560", "2:3": "2560x3840", "16:9": "3840x2160", "9:16": "2160x3840" },
};
const EXTRA_SIZES = {
  "1K": { "3:4": "1152x1536", "4:5": "1024x1280", "5:4": "1280x1024", "21:9": "2352x1008" },
  "2K": { "3:4": "1728x2304", "4:5": "1792x2240", "5:4": "2240x1792", "21:9": "3024x1296" },
  "4K": { "3:4": "2448x3264", "4:5": "2560x3200", "5:4": "3200x2560", "21:9": "3696x1584" },
};

// client-facing model -> tier, size family, default upstream model, accepted `quality` values.
// A channel "model mapping" overrides the default upstream model; the tier always follows the client-facing name.
const QUALITY_BASE = ["low", "medium", "high", "auto"];
const QUALITY_EXTENDED = ["low", "medium", "high", "xhigh", "max", "auto"];
const MODELS = {
  "gpt-image-2": { tier: "1K", extended: false, upstream: "gpt-image-2", quality: QUALITY_BASE },
  "gpt-image-2-2K": { tier: "2K", extended: false, upstream: "gpt-image-2-pro", quality: QUALITY_BASE },
  "gpt-image-2-4K": { tier: "4K", extended: false, upstream: "gpt-image-2-pro", quality: QUALITY_BASE },
  "gpt-image-2.5-1K": { tier: "1K", extended: true, upstream: "gpt-image-2.5-flare", quality: QUALITY_EXTENDED },
  "gpt-image-2.5-2K": { tier: "2K", extended: true, upstream: "gpt-image-2.5-flare", quality: QUALITY_EXTENDED },
  "gpt-image-2.5-4K": { tier: "4K", extended: true, upstream: "gpt-image-2.5-flare", quality: QUALITY_EXTENDED },
};

export const meta = {
  apiVersion: 1,
  key: "async-image-relay",
  name: "Async Image Relay",
  icon: "text:AI",
  description: {
    en: "Image generation through an upstream asynchronous images API, served on the task-style video endpoint",
    zh: "经上游异步出图接口生成图片，对外仍走任务式接口",
  },
  version: "1.0.0",
  author: { name: "Quriov" },
  models: Object.keys(MODELS),
  fetchMode: "per_task",
  protocols: ["openai_video"],
};

function trimmed(value) {
  return typeof value === "string" ? value.trim() : "";
}

function isObject(value) {
  return !!value && typeof value === "object" && !Array.isArray(value);
}

// `model` is the name the client sent; `upstreamModel` is that name after the channel's model mapping.
// The tier and size table follow whichever of the two is a model this plugin declares (client name first).
function resolveModel(model, upstreamModel) {
  if (MODELS[model]) return { name: model, spec: MODELS[model], upstream: upstreamModel && upstreamModel !== model ? upstreamModel : MODELS[model].upstream };
  if (MODELS[upstreamModel]) return { name: upstreamModel, spec: MODELS[upstreamModel], upstream: MODELS[upstreamModel].upstream };
  throw new Error("model " + String(model || "") + " is not served by this plugin");
}

function sizeTable(spec) {
  return spec.extended ? Object.assign({}, BASE_SIZES[spec.tier], EXTRA_SIZES[spec.tier]) : BASE_SIZES[spec.tier];
}

function parseSize(value) {
  const match = /^(\d{2,5})x(\d{2,5})$/.exec(trimmed(value).toLowerCase());
  return match ? { width: Number(match[1]), height: Number(match[2]) } : null;
}

// Resolves the `size` sent upstream from the client's `size` or `aspect_ratio` (top level or `metadata`).
function resolveSize(model, spec, req) {
  const table = sizeTable(spec);
  const metadata = isObject(req.metadata) ? req.metadata : {};
  for (const field of ["image_size", "resolution"]) {
    const requested = trimmed(req[field] === undefined ? metadata[field] : req[field]);
    if (requested && requested.toUpperCase() !== spec.tier)
      throw new Error(field + " " + requested + " does not match model " + model + " (" + spec.tier + "); choose the model of the tier you want instead");
  }
  if (req.size !== undefined && req.size !== null && req.size !== "") {
    const size = trimmed(req.size).toLowerCase();
    for (const ratio of Object.keys(table)) {
      if (table[ratio] === size) return size;
    }
    throw new Error("size " + String(req.size) + " is not supported by model " + model + "; supported sizes: " + Object.values(table).join(", "));
  }
  const requested = req.aspect_ratio === undefined ? metadata.aspect_ratio : req.aspect_ratio;
  if (requested !== undefined && requested !== null && typeof requested !== "string") throw new Error("aspect_ratio must be a string such as 16:9");
  let ratio = trimmed(requested).replace(/\s+/g, "");
  if (!ratio || ratio.toLowerCase() === "auto") ratio = "1:1";
  const size = table[ratio];
  if (!size) throw new Error("aspect_ratio " + ratio + " is not supported by model " + model + "; supported: " + Object.keys(table).join(", "));
  return size;
}

// Collects reference images. The upstream asynchronous API only accepts public http(s) URLs.
function referenceImages(req) {
  const metadata = isObject(req.metadata) ? req.metadata : {};
  const urls = [];
  for (const source of [req.images, req.image, req.input_reference, metadata.urls]) {
    if (source === undefined || source === null || source === "") continue;
    for (const item of Array.isArray(source) ? source : [source]) {
      const url = trimmed(item);
      if (!/^https?:\/\/\S+$/i.test(url))
        throw new Error("reference images must be public http(s) URLs; base64, data URIs and file uploads are not supported by this model");
      if (!urls.includes(url)) urls.push(url);
    }
  }
  if (urls.length > MAX_REFERENCE_IMAGES) throw new Error("at most " + MAX_REFERENCE_IMAGES + " reference images are supported, got " + urls.length);
  return urls;
}

// Validates the client request and returns everything the upstream request needs. Throws a readable Error.
function resolveRequest(model, upstreamModel, req) {
  if (!isObject(req)) throw new Error("request body must be a JSON object");
  const target = resolveModel(model, upstreamModel);
  const spec = target.spec;
  model = target.name;
  const prompt = trimmed(req.prompt);
  if (!prompt) throw new Error("field prompt is required");
  if (req.n !== undefined && req.n !== null && Number(req.n) !== 1)
    throw new Error("n must be 1: this model is billed per image, submit one request per image");
  const quality = trimmed(req.quality).toLowerCase();
  if (quality && !spec.quality.includes(quality))
    throw new Error("quality " + quality + " is not supported by model " + model + "; supported: " + spec.quality.join(", "));
  return {
    tier: spec.tier,
    upstreamModel: target.upstream,
    prompt: prompt,
    size: resolveSize(model, spec, req),
    quality: quality,
    images: referenceImages(req),
  };
}

function baseUrl(ctx) {
  return String(ctx.baseUrl || "").replace(/\/+$/, "");
}

function upstreamError(body) {
  if (!isObject(body) || body.error === undefined || body.error === null) return "";
  if (typeof body.error === "string") return trimmed(body.error);
  if (!isObject(body.error)) return "";
  const message = trimmed(body.error.message);
  const code = trimmed(body.error.code) || trimmed(body.error.type);
  if (message && code) return message + " (" + code + ")";
  return message || code;
}

export function buildSubmitRequest(ctx) {
  const resolved = resolveRequest(ctx.model, ctx.upstreamModel, ctx.requestBody);
  const body = {
    model: resolved.upstreamModel,
    prompt: resolved.prompt,
    n: 1,
    size: resolved.size,
    response_format: "url",
    watermark: false,
  };
  if (resolved.quality) body.quality = resolved.quality;
  if (resolved.images.length) body.image = resolved.images;
  return {
    url: baseUrl(ctx) + "/v1/images/generations/async",
    method: "POST",
    headers: { Authorization: "Bearer " + ctx.apiKey, "Content-Type": "application/json" },
    body: body,
    action: resolved.images.length ? "image_to_video" : "text_to_video",
  };
}

export function parseSubmitResponse(ctx, resp) {
  const body = resp.body;
  if (!isObject(body)) throw new Error("upstream returned a non-JSON submit response (HTTP " + resp.statusCode + ")");
  const failure = upstreamError(body);
  if (failure) throw new Error("upstream rejected the submission: " + failure);
  const status = trimmed(body.status).toLowerCase();
  if (status === "failed" || status === "cancelled") throw new Error("upstream rejected the submission: task " + status);
  const taskId = trimmed(body.id) || trimmed(body.task_id) || trimmed(body.taskId);
  if (!taskId) throw new Error("upstream accepted the submission but returned no task id");
  const resolved = resolveRequest(ctx.model, ctx.upstreamModel, ctx.requestBody);
  return {
    taskId: taskId,
    taskData: { status: status || "queued" },
    state: { submittedAt: utils.unixNow(), tier: resolved.tier, size: resolved.size },
  };
}

export function buildQueryRequest(ctx) {
  return {
    url: baseUrl(ctx) + "/v1/images/generations/async/" + encodeURIComponent(ctx.taskId),
    method: "GET",
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

function resultUrls(body) {
  const urls = [];
  if (!isObject(body) || !Array.isArray(body.data)) return urls;
  for (const item of body.data) {
    const url = isObject(item) ? trimmed(item.url) : "";
    if (/^https?:\/\//i.test(url)) urls.push(url);
  }
  return urls;
}

function taskTier(ctx) {
  const state = isObject(ctx.state) ? ctx.state : {};
  if (TIER_MIN_LONG_EDGE[state.tier]) return state.tier;
  const spec = MODELS[ctx.model] || MODELS[ctx.upstreamModel];
  return spec ? spec.tier : "";
}

// Returns "" when the delivered size satisfies the tier, otherwise the failure reason.
// The size is the one the upstream reports in its result object; this plugin cannot download the image.
function tierShortfall(tier, reported) {
  const minimum = TIER_MIN_LONG_EDGE[tier];
  if (!minimum) return "";
  const size = parseSize(reported);
  if (!size) {
    if (tier === "1K") return "";
    return "upstream result did not report the image size, so the " + tier + " tier cannot be confirmed";
  }
  const longEdge = Math.max(size.width, size.height);
  if (longEdge >= minimum) return "";
  return "upstream returned a " + size.width + "x" + size.height + " image, below the " + tier + " tier (long edge of at least " + minimum + " px)";
}

function progressText(value) {
  if (typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 100) return Math.round(value) + "%";
  const text = trimmed(value);
  return /^\d{1,3}%$/.test(text) ? text : "";
}

export function parseTaskResult(ctx, body, response) {
  const httpStatus = Number((response || {}).status || 0);
  const state = isObject(ctx.state) ? ctx.state : {};
  const submittedAt = Number(state.submittedAt || 0);
  const waited = submittedAt > 0 ? utils.unixNow() - submittedAt : 0;
  const overdue = waited > POLL_DEADLINE_SECONDS;
  const failure = upstreamError(body);
  const status = isObject(body) ? trimmed(body.status).toLowerCase() : "";
  const urls = resultUrls(body);

  // A finished task is the bare result object: `data[]` (+ `size`), usually without `status`.
  const finished = status === "completed" || status === "succeeded" || (!status && isObject(body) && Array.isArray(body.data));
  if (finished && httpStatus < 400 && !failure) {
    if (!urls.length) return { status: "FAILURE", reason: "upstream finished the task without an image URL" };
    const shortfall = tierShortfall(taskTier(ctx), body.size);
    if (shortfall) return { status: "FAILURE", reason: shortfall };
    return { status: "SUCCESS", progress: "100%", url: urls[0] };
  }
  if (status === "failed") return { status: "FAILURE", reason: "upstream task failed: " + (failure || "no reason given") };
  if (status === "cancelled") return { status: "FAILURE", reason: "upstream cancelled the task" + (failure ? ": " + failure : "") };

  const pending = { queued: "QUEUED", processing: "IN_PROGRESS", in_progress: "IN_PROGRESS" }[status];
  if (overdue) {
    const last = pending ? "last status " + status : failure || "unrecognized response (HTTP " + httpStatus + ")";
    return { status: "FAILURE", reason: "upstream did not finish within " + POLL_DEADLINE_SECONDS + " seconds (" + last + ")" };
  }
  if (pending && httpStatus < 400 && !failure) {
    const result = { status: pending };
    const progress = progressText(body.progress);
    if (progress && progress !== "100%") result.progress = progress;
    return result;
  }
  // Not recognizable as pending, finished or failed: the host counts it as a poll failure and fails the task
  // (with a refund) once the consecutive-failure limit is reached.
  return { status: "UNKNOWN", reason: failure || "unrecognized upstream status: " + (status || "HTTP " + httpStatus) };
}

// The finished image is a public URL on the upstream's CDN; artifact reads fetch it without channel credentials.
export function listArtifacts(task) {
  return task.status === "SUCCESS" && resultUrls(task.data).length ? [{ key: "image", type: "image" }] : [];
}

export function buildContentRequest(ctx) {
  const urls = resultUrls(ctx.data);
  if (ctx.artifactKey !== "image" || !urls.length) throw new Error("artifact_not_found");
  return { url: urls[0], method: ctx.clientRequest.method, credentialless: true };
}

export const protocols = {
  openai_video: {
    decodeRequest: function (ctx) {
      if (!ctx.body || ctx.body.kind !== "json")
        throw new Error("JSON body required; reference images must be passed as public http(s) URLs, file uploads are not supported by this model");
      const req = ctx.body.value;
      const resolved = resolveRequest(ctx.model, ctx.upstreamModel, req);
      return {
        kind: "submit",
        model: ctx.model,
        action: resolved.images.length ? "image_to_video" : "text_to_video",
        requestBody: Object.assign({}, req, { model: ctx.model }),
      };
    },
    // The host owns id / object / model / status / progress / created_at / completed_at.
    render: function (_ctx, task) {
      const status = String(task.status || "").toUpperCase();
      const output = {};
      if (status === "SUCCESS") {
        const urls = resultUrls(task.data);
        if (urls.length) {
          output.url = urls[0];
          output.image_url = urls[0];
          output.data = urls.map(function (url) {
            return { url: url };
          });
          output.metadata = { url: urls[0], image_url: urls[0] };
        }
        const size = isObject(task.data) ? trimmed(task.data.size) : "";
        if (size) output.size = size;
      }
      if (status === "FAILURE") output.error = { code: "image_generation_failed", message: trimmed(task.fail_reason) || "The image generation task failed." };
      return output;
    },
  },
};

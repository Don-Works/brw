(async function (options, key, resolve) {
  const originalScroll = { x: scrollX, y: scrollY };
  const cleanups = [];
  let cancelled = false;
  const assertActive = () => { if (cancelled) throw new Error("capture cancelled"); };
  window[key] = () => {
    cancelled = true;
    for (const cleanup of cleanups) cleanup();
    scrollTo({ left: originalScroll.x, top: originalScroll.y, behavior: "instant" });
    delete window[key];
  };

  const hidden = new Set();
  for (const selector of options.hide || []) {
    for (const element of document.querySelectorAll(selector)) {
      hidden.add(element);
      for (const child of element.querySelectorAll("*")) hidden.add(child);
    }
  }
  for (const element of hidden) {
    const value = element.style.getPropertyValue("visibility");
    const priority = element.style.getPropertyPriority("visibility");
    const hadStyle = element.hasAttribute("style");
    cleanups.push(() => {
      if (value) element.style.setProperty("visibility", value, priority);
      else element.style.removeProperty("visibility");
      if (!hadStyle && element.getAttribute("style") === "") element.removeAttribute("style");
    });
    element.style.setProperty("visibility", "hidden", "important");
  }

  let box;
  if (options.ref) {
    box = resolve(options.ref);
    if (!box.ok) throw new Error("screenshot ref not found: " + options.ref);
  }
  const visible = (element) => {
    const rect = element.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0 && getComputedStyle(element).visibility !== "hidden" &&
      (options.full_page || options.ref || rect.bottom > 0 && rect.right > 0 && rect.top < innerHeight && rect.left < innerWidth);
  };
  const bounded = async (promise, milliseconds, label) => {
    let timer;
    try {
      return await Promise.race([
        promise,
        new Promise((_, reject) => {
          timer = setTimeout(() => reject(new Error(label + " did not settle")), milliseconds);
        })
      ]);
    } finally {
      clearTimeout(timer);
    }
  };
  const images = Array.from(document.images).filter(visible);
  for (const image of images) {
    if (image.loading !== "lazy") continue;
    cleanups.push(() => { image.loading = "lazy"; });
    image.loading = "eager";
  }
  const videos = Array.from(document.querySelectorAll("video")).filter(video => visible(video) && (video.currentSrc || video.srcObject));
  await bounded(Promise.all([
    document.fonts.ready,
    ...images.map(image => image.decode()),
    ...videos.map(video => new Promise((resolve, reject) => {
      if (video.error) {
        reject(new Error("video failed to load"));
        return;
      }
      if (video.paused && video.readyState >= 2) {
        resolve();
        return;
      }
      if (video.requestVideoFrameCallback) {
        const id = video.requestVideoFrameCallback(resolve);
        cleanups.push(() => video.cancelVideoFrameCallback(id));
      } else {
        const ready = () => { if (video.readyState >= 2) resolve(); };
        video.addEventListener("loadeddata", ready);
        cleanups.push(() => video.removeEventListener("loadeddata", ready));
        ready();
      }
    }))
  ]), 5000, "fonts/images/video");
  assertActive();
  if (options.settle_ms) await new Promise(resolve => setTimeout(resolve, options.settle_ms));
  assertActive();
  await bounded(new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))), 2000, "compositor");
  assertActive();
  if (options.ref) {
    box = resolve(options.ref);
    if (!box.ok) throw new Error("screenshot ref disappeared");
  }

  let clip;
  if (options.full_page) {
    const root = document.documentElement;
    const body = document.body;
    clip = {
      x: 0, y: 0,
      width: Math.max(root.scrollWidth, body?.scrollWidth || 0, innerWidth),
      height: Math.max(root.scrollHeight, body?.scrollHeight || 0, innerHeight)
    };
  } else if (box) {
    clip = { x: box.x, y: box.y, width: box.width, height: box.height };
  } else if (options.region) {
    clip = { ...options.region, x: options.region.x + scrollX, y: options.region.y + scrollY };
  } else {
    clip = { x: scrollX, y: scrollY, width: innerWidth, height: innerHeight };
  }
  const width = Math.round(clip.width * options.scale);
  const height = Math.round(clip.height * options.scale);
  if (width < 1 || height < 1 || width * height > 33554432) {
    throw new Error("screenshot must be between 1 pixel and 32 megapixels");
  }
  let crop;
  if (![clip.x, clip.y, clip.width, clip.height].every(Number.isInteger)) {
    const x = Math.floor(clip.x);
    const y = Math.floor(clip.y);
    crop = { x: Math.round((clip.x - x) * options.scale), y: Math.round((clip.y - y) * options.scale), width, height };
    clip = { x, y, width: Math.ceil(clip.x + clip.width) - x + 1, height: Math.ceil(clip.y + clip.height) - y + 1 };
  }
  if (clip.width * clip.height * options.scale * options.scale > 33554432) {
    throw new Error("enclosing screenshot exceeds 32 megapixels");
  }
  clip.scale = options.scale / devicePixelRatio;
  return { clip, crop, width, height };
})

(async function (data, crop, format, quality) {
  const binary = atob(data);
  const bytes = Uint8Array.from(binary, character => character.charCodeAt(0));
  const bitmap = await createImageBitmap(new Blob([bytes], { type: "image/png" }));
  try {
    const canvas = new OffscreenCanvas(crop.width, crop.height);
    const context = canvas.getContext("2d");
    context.drawImage(bitmap, crop.x, crop.y, crop.width, crop.height, 0, 0, crop.width, crop.height);
    const blob = await canvas.convertToBlob({ type: "image/" + format, quality: quality / 100 });
    const encoded = new Uint8Array(await blob.arrayBuffer());
    const chunks = [];
    for (let offset = 0; offset < encoded.length; offset += 8192) {
      chunks.push(String.fromCharCode(...encoded.subarray(offset, offset + 8192)));
    }
    return btoa(chunks.join(""));
  } finally {
    bitmap.close();
  }
})

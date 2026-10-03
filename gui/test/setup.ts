// jsdom には canvas がないので、プレビューは描かない（描画そのものは preview.test.ts で確かめる）
HTMLCanvasElement.prototype.getContext = (() => null) as any

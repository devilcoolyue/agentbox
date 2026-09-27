import assert from 'node:assert/strict';

export async function assertActionIcons(page, root = 'body') {
  const malformed = await page.locator(root).evaluate(container => [...container.querySelectorAll('svg.ui-icon')]
    .filter(svg => svg.getAttribute('viewBox') !== '0 0 24 24' || svg.querySelectorAll(':scope > path[d]').length !== 1 || svg.querySelector('svg'))
    .map(svg => svg.dataset.iconName || svg.outerHTML));
  assert.deepEqual(malformed, [], 'shared icons and SVG state slots must render exactly one path');
  const failures = await page.locator(root).evaluate(container => [...container.querySelectorAll('button.btn, button.dlg-x')]
    .filter(button => !button.classList.contains('loading'))
    .flatMap(button => {
      const name = button.id || button.getAttribute('aria-label') || button.textContent;
      const icons = button.querySelectorAll(':scope > svg');
      const problems = [];
      if (icons.length !== 1 || !icons[0]?.querySelector('path[d]')) problems.push(`${name}: missing or duplicate icon`);
      if (!button.textContent.trim() && (!button.getAttribute('aria-label') || !button.dataset.tip)) problems.push(`${name}: missing accessible name or tooltip`);
      if (button.matches('.action-icon, .action-tools > .action-control') && button.getClientRects().length) {
        if (button.innerText.trim()) problems.push(`${name}: action text is visible`);
        if (!button.getAttribute('aria-label') || !button.dataset.tip) problems.push(`${name}: icon action has no name or tooltip`);
        const bounds = button.getBoundingClientRect(), icon = icons[0]?.getBoundingClientRect();
        if (icon && (Math.abs(bounds.x + bounds.width / 2 - icon.x - icon.width / 2) > 1 || Math.abs(bounds.y + bounds.height / 2 - icon.y - icon.height / 2) > 1)) problems.push(`${name}: icon is off center`);
        if (Math.abs(bounds.width - bounds.height) > 1) problems.push(`${name}: action is not square`);
      }
      return problems;
    }));
  assert.deepEqual(failures, []);
}

export async function fileActionSmoke(page) {
  for (const width of [1440, 390]) {
    await page.setViewportSize({width, height:900});
    for (const [scope, label] of [['ws', '空间文件'], ['shared', '共享目录']]) {
      await page.locator('#scope-' + scope).click();
      for (const id of ['mkdir', 'upload', 'download']) {
        assert.equal(await page.locator('#btn-' + id).innerText(), '');
        assert.ok(await page.locator('#btn-' + id).getAttribute('aria-label'));
        if (width <= 760) assert.ok((await page.locator('#btn-' + id).boundingBox()).height >= 44);
      }
      assert.match(await page.locator('#btn-download').getAttribute('data-tip'), new RegExp(label + '.*ZIP'));
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await assertActionIcons(page);
    }
  }
  await page.setViewportSize({width:1280, height:900});
  await page.locator('#scope-ws').click();
  await page.locator('#btn-download').hover();
  await page.locator('#tip.show').filter({hasText:'下载全部空间文件（ZIP）'}).waitFor();
  await page.keyboard.press('Escape');
  await page.locator('#btn-upload').focus();
  await page.locator('#tip.show').filter({hasText:'上传代码包并解压到空间文件'}).waitFor();
  await page.keyboard.press('Escape');
}

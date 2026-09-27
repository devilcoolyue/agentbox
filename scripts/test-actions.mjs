import assert from 'node:assert/strict';

export async function assertActionIcons(page, root = 'body') {
  const failures = await page.locator(root).evaluate(container => [...container.querySelectorAll('button.btn, button.dlg-x')]
    .filter(button => !button.classList.contains('loading'))
    .flatMap(button => {
      const name = button.id || button.getAttribute('aria-label') || button.textContent;
      const icons = button.querySelectorAll(':scope > svg');
      const problems = [];
      if (icons.length !== 1 || !icons[0]?.querySelector('path[d]')) problems.push(`${name}: missing or duplicate icon`);
      if (!button.textContent.trim() && (!button.getAttribute('aria-label') || !button.dataset.tip)) problems.push(`${name}: missing accessible name or tooltip`);
      return problems;
    }));
  assert.deepEqual(failures, []);
}

export async function fileActionSmoke(page) {
  for (const width of [1440, 390]) {
    await page.setViewportSize({width, height:900});
    for (const [scope, label] of [['ws', '空间文件'], ['shared', '共享目录']]) {
      await page.locator('#scope-' + scope).click();
      for (const [id, text] of [['mkdir', '新建'], ['upload', '上传'], ['download', '下载']]) {
        assert.equal(await page.locator('#btn-' + id).innerText(), text);
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

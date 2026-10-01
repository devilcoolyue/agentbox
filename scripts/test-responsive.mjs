import assert from 'node:assert/strict';

export async function responsiveSmoke(page, base) {
  const choose = async label => {
    await page.locator('#mobile-section .select-trigger').click();
    await page.locator('.select-panel:popover-open [role=option]').filter({hasText:label}).click();
    assert.equal(await page.locator('.select-panel:popover-open').count(), 0);
  };
  for (const width of [360, 390, 430, 768, 1280, 1440]) {
    await page.setViewportSize({width,height:900});
    for (const theme of ['light','dark']) {
      await page.goto(base + '/#/settings/accounts');
      await page.locator('#sec-accounts').waitFor({state:'visible'});
      await page.evaluate(theme => document.documentElement.dataset.theme=theme, theme);
      if (width <= 760) {
        assert.equal(await page.locator('#set-nav').isVisible(), false);
        assert.equal(await page.locator('#view-settings > .set-head').isVisible(), false);
        assert.ok((await page.locator('#acct-list-box').boundingBox()).y < 160, 'navigation consumes content space');
        for (const [label, sec] of [['IP 代理','proxies'],['容器与资源','container'],['模型管理','models'],['价目表','pricing'],['界面与提示','interface'],['安全与访问','security'],['运维监控','monitor'],['关于与更新','about'],['账号池','accounts']]) {
          await choose(label);
          await page.locator('#sec-'+sec).waitFor({state:'visible'});
          assert.equal(await page.locator('#topbar-title').innerText(),'系统设置');
          assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth), sec+' page overflow');
        }
        await page.locator('#mobile-section .select-trigger').click();
        await page.keyboard.press('Escape');
        assert.equal(await page.locator('.select-panel:popover-open').count(),0);
        await page.locator('#sec-accounts .section-help summary').click();
        assert.equal(await page.locator('#sec-accounts .section-help p').isVisible(),true);
        await page.locator('#sec-accounts .section-help summary').click();
      } else {
        assert.equal(await page.locator('#set-nav').isVisible(),true);
        assert.equal(await page.locator('#sec-accounts .section-help p').isVisible(),true);
      }
      await page.locator('#toast.show').waitFor({state:'hidden'});
      assert.ok(await page.locator('#acct-list-box').evaluate(e=>e.scrollWidth<=e.clientWidth), 'account contents overflow');
      assert.equal(await page.locator('#btn-acct-add').innerText(), '添加账号', 'standalone creation action needs a label');
      // Account rows show their two common actions with words; the rest live in ⋯.
      for (const row of await page.locator('.acct-row').all()) {
        assert.deepEqual(await row.locator('.acct-actions .btn').allInnerTexts(), ['认证', '编辑', ''], 'account row actions');
        assert.ok(await row.locator('.acct-actions .more-btn').getAttribute('aria-label'));
      }
      await page.screenshot({animations:'disabled',path:`output/playwright/responsive-settings-${width}-${theme}.png`});
      if (width > 760) {
        // Git management sits with the other sidebar tools, labelled; the user button names the user.
        assert.equal(await page.locator('#btn-git-management').innerText(), 'Git 管理');
        assert.ok((await page.locator('#side-user-label').innerText()).length > 0);
        await page.locator('#btn-user-menu').click();
        await page.screenshot({animations:'disabled',path:`output/playwright/context-menu-${width}-${theme}.png`});
        await page.keyboard.press('Escape');
      }
      await page.goto(base + '/#/settings/models');
      await page.locator('#sec-models').waitFor({state:'visible'});
      for (const button of await page.locator('.model-row .m-reasoning').all()) {
        assert.match(await button.innerText(), /^模型能力 · /, 'model capability description is visible');
      }
      assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth), 'model descriptions overflow');
      await page.screenshot({animations:'disabled',path:`output/playwright/context-models-${width}-${theme}.png`});
      for (const view of ['usage','tunnel','git/guide']) {
        await page.goto(base + '/#/'+view);
        await page.locator('#view-'+view.split('/')[0]).waitFor({state:'visible'});
        assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth), view+' page overflow');
      }
      if (width <= 760) {
        await choose('提交身份');
        await page.waitForURL('**/#/git/profile');
        await choose('仓库连接');
        await page.waitForURL('**/#/git/connections');
        await page.goBack();
        await page.waitForURL('**/#/git/profile');
        assert.match(await page.locator('#mobile-section .select-trigger').innerText(),/提交身份/);
        await page.reload();
        await page.locator('#view-git').waitFor({state:'visible'});
        assert.match(await page.locator('#mobile-section .select-trigger').innerText(),/提交身份/);
      }
    }
  }
  await page.setViewportSize({width:1280,height:900});
  console.log('Responsive: six widths, both themes, all settings sections, Git picker/history/reload, help, Escape, no page overflow passed');
}

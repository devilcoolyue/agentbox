import assert from 'node:assert/strict';

export async function responsiveSmoke(page, base) {
  const choose = async label => {
    await page.locator('#mobile-section-btn').click();
    await page.locator('#mobile-section-menu:popover-open [role=menuitemradio]').filter({hasText:label}).click();
    assert.equal(await page.locator('#mobile-section-menu:popover-open').count(), 0);
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
        // 分区菜单一次列全：没有搜索框（获焦会弹键盘），竖屏手机（最矮 iPhone SE 667px）也不用在菜单里滑动。
        for (const height of [900, 667]) {
          await page.setViewportSize({width,height});
          await page.locator('#mobile-section-btn').click();
          // 定位与聚焦排在打开后的下一帧；机器快时直接读会早于那一帧
          await page.waitForFunction(()=>document.activeElement?.closest('#mobile-section-menu'));
          const menu = await page.locator('#mobile-section-menu:popover-open').evaluate(menu => {
            const r = menu.getBoundingClientRect();
            return {items:menu.querySelectorAll('[role=menuitemradio]').length, inputs:menu.querySelectorAll('input').length,
              scrolls:menu.scrollHeight>menu.clientHeight, inView:r.bottom<=innerHeight && r.left>=0 && r.right<=innerWidth,
              checked:menu.querySelector('[aria-checked=true] .action-label')?.textContent.trim(), focused:document.activeElement?.getAttribute('aria-checked'),
              ids:menu.querySelectorAll('[id]').length};
          });
          assert.deepEqual(menu, {items:9, inputs:0, scrolls:false, inView:true, checked:'账号池', focused:'true', ids:0}, 'section menu shows every section at '+height+'px');
          await page.keyboard.press('ArrowDown');
          assert.match(await page.evaluate(()=>document.activeElement.textContent), /IP 代理/);
          await page.keyboard.press('Escape');
          assert.equal(await page.locator('#mobile-section-menu:popover-open').count(),0);
          assert.equal(await page.locator('#mobile-section-btn').getAttribute('aria-expanded'),'false');
        }
        await page.setViewportSize({width,height:900});
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
        // 收起侧栏：工具图标与会话头像都在窄栏正中；用户弹层不受窄栏规则影响，「连接延迟」横排、读数可见。
        await page.locator('#btn-sidebar-toggle').click();
        // 侧栏宽度有 0.2s 过渡：等它走完再量，过渡中途量到的是半截宽度
        const sidebarSettled = narrow => page.waitForFunction(narrow => {
          const side = document.querySelector('#sidebar'), width = side.getBoundingClientRect().width;
          return (narrow ? width < 80 : width > 100) && !side.getAnimations({subtree:true}).some(a => a instanceof CSSTransition);
        }, narrow);
        await sidebarSettled(true);
        const rail = await page.evaluate(()=>{
          const side = document.querySelector('#sidebar').getBoundingClientRect();
          const mid = side.left + (side.width - 1) / 2; // 去掉右边框
          const off = el => { const r = el.querySelector('.ui-icon, .sc-initial').getBoundingClientRect(); return Math.round(Math.abs(r.left + r.width/2 - mid)); };
          return Math.max(...[...document.querySelectorAll('.side-tools > .side-tool:not(.hidden), #session-list .session-card')].map(off));
        });
        assert.ok(rail <= 1, 'collapsed sidebar icons are off-centre by '+rail+'px');
        await page.locator('#btn-user-menu').click();
        const conn = await page.evaluate(()=>{
          const label = document.querySelector('.side-user-conn .mq-label').getBoundingClientRect();
          const text = document.querySelector('#conn-text');
          return {oneLine:label.height<24, text:getComputedStyle(text).display!=='none'};
        });
        assert.deepEqual(conn, {oneLine:true, text:true}, 'collapsed sidebar user menu latency row');
        await page.screenshot({animations:'disabled',path:`output/playwright/collapsed-menu-${width}-${theme}.png`});
        await page.keyboard.press('Escape');
        await page.locator('#btn-sidebar-toggle').click();
        await sidebarSettled(false);
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
        assert.match(await page.locator('#mobile-section-btn').innerText(),/提交身份/);
        await page.reload();
        await page.locator('#view-git').waitFor({state:'visible'});
        assert.match(await page.locator('#mobile-section-btn').innerText(),/提交身份/);
      }
    }
  }
  await page.setViewportSize({width:1280,height:900});
  console.log('Responsive: six widths, both themes, all settings sections, Git picker/history/reload, help, Escape, no page overflow passed');
}

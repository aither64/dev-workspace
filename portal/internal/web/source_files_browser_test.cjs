const assert = require("node:assert/strict");
const fs = require("node:fs");

(async () => {
  const source = fs.readFileSync("static/source-file.js", "utf8");
  const {sourceFileURL, sourceFileLine, sourceFileVersion} = await import("data:text/javascript;base64," + Buffer.from(source).toString("base64"));
  const original = "https://workspace.example.test/files/example?repository=abc&path=a%20%23%25.rb";
  for (const line of [1, 10, 12000, Number.MAX_SAFE_INTEGER]) {
    const href = sourceFileURL(original, line);
    assert.equal(sourceFileLine(href), line);
    assert.equal(new URL(href).search, new URL(original).search);
  }
  for (const hash of ["", "#L0", "#L-1", "#L01", "#L1x", "#old-L1", "#L9007199254740992"]) {
    assert.equal(sourceFileLine(original + hash), null, hash);
  }
  assert.equal(sourceFileURL(original + "#L10", null), original);
  assert.match(sourceFileVersion({source: "archive", revision: "exact-final-head"}), /exact-final-head/);
  assert.match(sourceFileVersion({source: "worktree"}), /uncommitted/);
  assert.match(sourceFileVersion({source: "archived-artifact"}), /Archived/);
  assert.equal(sourceFileVersion({source: "artifact"}), "Current session file");
  assert.match(sourceFileVersion({source: "workspace"}), /Current workspace.*uncommitted/);
  assert.throws(() => sourceFileVersion({source: "unknown"}), /source is unknown/);

  // Replace only the existing deferred editor module, retaining the shipped
  // renderer, mounted controls and generation checks.
  const editorImport = 'await import("/static/review-editor.js")';
  assert(source.includes(editorImport));
  const mountedSource = source.replace(editorImport, 'await globalThis.sourceEditorImport()');
  const {renderSourceFileContent, mountSourceFile} = await import("data:text/javascript;base64," + Buffer.from(mountedSource).toString("base64"));
  class Node {
    constructor() { this.children=[]; this.handlers={}; this.textContent=""; this.hidden=false; this.dataset={};
      this.classes=new Set(); this.classList={add:(...names)=>names.forEach(name=>this.classes.add(name)),
        remove:(...names)=>names.forEach(name=>this.classes.delete(name)), contains:name=>this.classes.has(name), toggle:(name,on)=>on?this.classes.add(name):this.classes.delete(name)}; }
    append(...nodes) { for (const node of nodes) {node.parent=this; this.children.push(node);} }
    replaceChildren(...nodes) { this.children=[]; this.append(...nodes); }
    remove() { if(this.parent) this.parent.children=this.parent.children.filter(node=>node!==this); }
    addEventListener(name, fn) { this.handlers[name]=fn; }
    setAttribute(name,value) { (this.attributes??={})[name]=value; }
    removeAttribute(name) { delete this.attributes?.[name]; }
    querySelectorAll(selector) {
      const all=this.children.flatMap(node=>[node,...node.querySelectorAll(selector)]);
      return all.filter(node=>selector==="[data-file-path]"?node.dataset.filePath!==undefined:
        selector==="[data-file-directory]"?node.dataset.fileDirectory!==undefined:false);
    }
  }
  const elements = Object.fromEntries(["notice", "content", "title", "version", "copy", "copy-content", "download", "mode"].map(name =>
    [`source-file-${name}`, new Node()]));
  let copied, editorOptions=[], destroyed=0;
  Object.defineProperty(globalThis, "navigator", {value: {clipboard: {writeText: async text => {copied = text;}}}});
  const events = {}, documentEvents={};
  globalThis.document = {hidden:false, createElement: () => new Node(), getElementById: id => elements[id],
    querySelector: () => ({content: "nonce"}), addEventListener: (name,fn) => {documentEvents[name]=fn;}};
  globalThis.location = new URL(original);
  globalThis.addEventListener = (name, callback) => { events[name] = callback; };
  globalThis.history = {replaceState(_state,_title,href) { globalThis.location=new URL(href); }};
  globalThis.sourceEditorImport = async () => ({createReviewEditor: options => {
    editorOptions.push(options);
    return {clearLine() {}, revealLine: async () => true, destroy() {destroyed++;}};
  }});
  const parent=new Node(), raw={path:"report.md",source:"artifact",content:{text:"# Original bytes\r\n"},preview:{kind:"markdown",html:"<h1>Original bytes</h1>"}};
  let current=true;
  const preview=await renderSourceFileContent({parent,file:raw,mode:"preview",isCurrent:()=>current});
  assert.equal(parent.children[0].innerHTML,raw.preview.html);assert.equal(editorOptions.length,0);
  preview.destroy();assert.equal(parent.children.length,0);
  const passive=await renderSourceFileContent({parent,file:{...raw,preview:undefined,content:{text:'<svg onload="bad()"></svg>'}},mode:"raw",isCurrent:()=>current});
  assert.equal(editorOptions.at(-1).after,'<svg onload="bad()"></svg>');assert.equal(parent.children[0].innerHTML,undefined);
  passive.destroy();assert.equal(destroyed,1);
  for(const content of [{binary:true},{limited:true,bytes:999999}]) {
    const view=await renderSourceFileContent({parent,file:{path:"large.bin",content},mode:"raw",isCurrent:()=>current});
    assert.match(parent.children[0].children[0].textContent,/Download|Preview omitted/);view.destroy();
  }
  await assert.rejects(renderSourceFileContent({parent,file:{...raw,preview:{kind:"image",url:"https://other.test/image.png"}},mode:"preview",isCurrent:()=>current}),/unavailable/);
  const image=await renderSourceFileContent({parent,file:{...raw,preview:{kind:"image",url:"/artifact-previews/example/image.png"}},mode:"preview",isCurrent:()=>current});
  const imageNode=parent.children[0].children[0];assert.match(imageNode.src,/\/artifact-previews\/example\/image.png$/);
  current=false;imageNode.handlers.error();assert.equal(parent.children[0].children[0],imageNode);image.destroy();current=true;

  let releaseImport;
  const originalImporter=globalThis.sourceEditorImport;
  globalThis.sourceEditorImport=()=>new Promise(resolve=>{releaseImport=resolve;});
  const stale=renderSourceFileContent({parent,file:raw,mode:"raw",isCurrent:()=>current});
  current=false;releaseImport(await originalImporter());await stale;
  assert.equal(parent.children.length,0,"A retired editor import must not attach DOM");
  globalThis.sourceEditorImport=originalImporter;current=true;

  let fetchedURL, fetches=0;
  globalThis.fetch=async url=>{fetchedURL=url;fetches++;return {ok:true,json:async()=>raw};};
  globalThis.location=new URL(original);
  await mountSourceFile({dataset:{slug:"example"}});
  assert.equal(fetchedURL,`/api/sessions/example/file${location.search}`);
  assert.equal(elements["source-file-content"].children[0].innerHTML,raw.preview.html);
  await elements["source-file-copy-content"].handlers.click();assert.equal(copied,raw.content.text);
  assert.equal(elements["source-file-download"].href,`/api/sessions/example/file/download${location.search}`);
  elements["source-file-mode"].handlers.click();
  await new Promise(setImmediate);
  assert.equal(editorOptions.at(-1).after,raw.content.text);assert.equal(fetches,1,"Raw toggle must reuse fetched bytes");
  assert.equal(elements["source-file-mode"].ariaLabel,"Show preview");
  globalThis.location=new URL(original+"#L10");
  let finishOldReveal;
  globalThis.sourceEditorImport=async()=>({createReviewEditor:()=>({clearLine() {},destroy() {},revealLine:(_side,line)=>line===10
    ?new Promise(resolve=>{finishOldReveal=resolve;}):Promise.resolve(false)})});
  events.hashchange();
  for(let tries=0;!finishOldReveal&&tries<50;tries++) await new Promise(setImmediate);
  assert.equal(typeof finishOldReveal,"function");
  globalThis.location=new URL(original+"#L999");events.hashchange();await new Promise(setImmediate);
  assert.match(elements["source-file-content"].children[0].children[0].textContent,/Line 999 is not present/);
  finishOldReveal(true);await new Promise(setImmediate);
  assert.match(elements["source-file-content"].children[0].children[0].textContent,/Line 999 is not present/);

  // Old servers omit preview; workspace/repository line links still select raw.
  globalThis.sourceEditorImport=originalImporter;
  globalThis.location=new URL("https://workspace.example.test/workspace-files?path=docs%2Fguide.md#L93");
  let revealedLine;
  globalThis.sourceEditorImport=async()=>({createReviewEditor:options=>({revealLine:async(_side,line)=>{revealedLine=line;return true;},destroy() {}})});
  globalThis.fetch=async url=>{fetchedURL=url;return {ok:true,json:async()=>({path:"docs/guide.md",source:"workspace",content:{text:""}})};};
  await mountSourceFile({dataset:{fileApi:"/api/workspace/file"}});
  assert.equal(fetchedURL,"/api/workspace/file?path=docs%2Fguide.md");assert.equal(revealedLine,93);
  assert.match(elements["source-file-version"].textContent,/Current workspace/);
  assert.equal(elements["source-file-copy-content"].disabled,false);
  await elements["source-file-copy-content"].handlers.click();assert.equal(copied,"");
  assert.equal(elements["source-file-mode"].hidden,true);
  for(const content of [{binary:true},{limited:true,bytes:999999}]) {
    globalThis.fetch=async()=>({ok:true,json:async()=>({path:"large.bin",source:"artifact",content})});
    await mountSourceFile({dataset:{slug:"example"}});
    assert.equal(elements["source-file-copy-content"].disabled,true);
  }
  // Execute the shipped inline Files block with controlled request completion.
  // This retains its real read scopes, selection guards, toolbar and event logic.
  const appSource=fs.readFileSync("static/app.js","utf8");
  const filesBlock=appSource.slice(appSource.indexOf('  const artifactList = document.getElementById("session-files");'),appSource.indexOf('  const pageReads = createReadScope();'));
  const inlineElements=Object.fromEntries(["session-files","files-status","files-refresh","files-content","files-title","files-content-status","files-mode","files-copy","files-download","files"].map(id=>[id,new Node()]));
  inlineElements.files.classList.add("active");
  globalThis.document={hidden:false,createElement:()=>new Node(),createDocumentFragment:()=>new Node(),
    getElementById:id=>inlineElements[id],querySelector:()=>null,addEventListener:(name,fn)=>{documentEvents[name]=fn;}};
  const rootRows=[{path:"alpha",kind:"directory"},{path:"beta",kind:"directory"},{path:"gamma",kind:"directory"},
    {path:"a.md",kind:"file"},{path:"b.txt",kind:"file"}];
  let releases={}, inlineRequests=[], directoryRequests=[];
  const client={
    directory:(path,cursor,options)=>new Promise((resolve,reject)=>{
      const request={path,cursor,options,resolve,reject}; directoryRequests.push(request);
      if(directoryRequests.length===1) resolve({path:"",entries:rootRows,nextCursor:"root-more"});
    }),
    file:(path,options)=>{inlineRequests.push({path,options});return new Promise((resolve,reject)=>{releases[path]={resolve,reject};});}
  };
  const inlineModule={renderSourceFileContent};
  const {createReadScope}=require("./static/app.js");
  const mountInline=new Function("document","client","slug","createReadScope","refreshPolicy","inlineModule","apiPath",
    filesBlock.replace('await import("/static/source-file.js?v=3")','inlineModule')+'return {selectFile,pauseFiles,resumeFiles,refreshFiles,directories,rootDirectory};');
  globalThis.sourceEditorImport=originalImporter;
  const inline=mountInline(document,client,"example",createReadScope,{detailsDeadlineMs:10000},inlineModule,(slug,operation)=>`/api/sessions/${encodeURIComponent(slug)}/${operation}`);
  const settle=async()=>{for(let i=0;i<3;i++) await new Promise(setImmediate);};
  const directory=path=>inline.directories.get(path);
  const toggle=(path,open)=>{directory(path).details.open=open;directory(path).details.handlers.toggle();};
  const lastRequest=path=>directoryRequests.filter(request=>request.path===path).at(-1);
  const answer=async(path,entries,nextCursor=null)=>{lastRequest(path).resolve({path,entries,nextCursor});await settle();};
  const statusButton=path=>directory(path).status.children.find(node=>node.textContent==="Load more"||node.textContent==="Retry"||node.textContent==="Reload directory");
  await settle();
  assert.equal(directoryRequests.length,1,"Initial load must read root only");
  for(const node of inlineElements["session-files"].querySelectorAll("[data-file-directory]")) assert(!node.open,"Directories start collapsed");
  const links=inlineElements["session-files"].querySelectorAll("[data-file-path]");
  let prevented=false;
  links[0].handlers.click({button:0,ctrlKey:true,preventDefault(){prevented=true;}});
  assert.equal(prevented,false);assert.equal(inlineRequests.length,0,"Modified anchors must keep native behavior");
  links[0].handlers.click({button:0,preventDefault(){prevented=true;}});
  assert.equal(prevented,true);assert.equal(inlineRequests[0].path,"a.md");
  inline.selectFile("b.txt");assert.equal(inlineRequests[0].options.signal.aborted,true);
  const selected={path:"b.txt",source:"artifact",content:{text:"canonical B\r\n"}};
  releases["b.txt"].resolve(selected);await settle();
  releases["a.md"].resolve({...raw,path:"a.md"});await settle();
  assert.equal(inlineElements["files-title"].textContent,"b.txt");
  assert.equal(inlineElements["files-download"].href,"/api/sessions/example/file/download?artifact=b.txt");
  assert.equal(editorOptions.at(-1).after,"canonical B\r\n");
  assert.equal(editorOptions.at(-1).lineURL("new",93),"/files/example?artifact=b.txt#L93");
  await inlineElements["files-copy"].handlers.click();assert.equal(copied,"canonical B\r\n");
  const retainedBody=inlineElements["files-content"].children[0];inlineElements["session-files"].scrollTop=80;
  statusButton("").handlers.click();assert.equal(lastRequest("").cursor,"root-more");
  await answer("",[{path:"c.txt",kind:"file"}]);
  assert.equal(inlineElements["session-files"].querySelectorAll("[data-file-path]").length,3);
  assert.equal(inlineElements["files-content"].children[0],retainedBody);
  assert.equal(inlineElements["session-files"].scrollTop,80);

  toggle("alpha",true);toggle("beta",true);toggle("gamma",true);
  assert.deepEqual(directoryRequests.slice(-2).map(request=>request.path),["alpha","beta"],"Only two directory reads may run");
  const cancelledAlpha=lastRequest("alpha");toggle("alpha",false);
  assert(cancelledAlpha.options.signal.aborted);assert.equal(inlineElements["files-title"].textContent,"b.txt");
  cancelledAlpha.resolve({path:"alpha",entries:[{path:"alpha/late",kind:"file"}],nextCursor:null});await settle();
  assert.equal(directory("alpha").entries.length,0,"Late collapsed success must not attach rows");
  assert.equal(lastRequest("gamma").path,"gamma","Cancelling frees the slot for the queued directory");
  await answer("beta",[{path:"beta/old",kind:"file"}]);await answer("gamma",[]);
  toggle("alpha",true);
  await answer("alpha",[{path:"alpha/nested",kind:"directory"},{path:"alpha/first",kind:"file"}],"alpha-more");
  assert(!directory("alpha/nested").details.open);assert(!directoryRequests.some(request=>request.path==="alpha/nested"));
  statusButton("alpha").handlers.click();
  lastRequest("alpha").reject(Object.assign(new Error("changed"),{status:409,code:"directory_changed"}));await settle();
  assert.equal(directory("alpha").entries.length,2);assert.equal(directory("alpha").cursor,null);
  assert.equal(statusButton("alpha").textContent,"Reload directory");assert.equal(inlineElements["files-content"].children[0],retainedBody);
  const beforeReload=directoryRequests.length;await settle();assert.equal(directoryRequests.length,beforeReload,"409 must not restart itself");
  statusButton("alpha").handlers.click();assert.equal(lastRequest("alpha").cursor,null);
  await answer("alpha",[{path:"alpha/nested",kind:"directory"},{path:"alpha/first",kind:"file"}],"alpha-more");
  statusButton("alpha").handlers.click();await answer("alpha",[{path:"alpha/second",kind:"file"}]);
  assert.equal(directory("alpha").entries.length,3);

  toggle("alpha/nested",true);const cancelledNested=lastRequest("alpha/nested");
  toggle("alpha",false);assert(cancelledNested.options.signal.aborted,"Collapsing cancels the whole subtree");
  cancelledNested.reject(new Error("late failure"));await settle();
  assert.equal(directory("alpha/nested").error,"","Late errors must not change collapsed state");
  toggle("alpha",true);await answer("alpha/nested",[]);
  toggle("beta",false);
  // Hidden reads are cancelled; reopening retries only the visible unfinished node.
  inline.refreshFiles();
  assert.deepEqual(directoryRequests.slice(-2).map(request=>request.path),["","alpha"]);
  const hiddenRoot=lastRequest(""),hiddenAlpha=lastRequest("alpha");
  document.hidden=true;inline.pauseFiles();assert(hiddenRoot.options.signal.aborted);assert(hiddenAlpha.options.signal.aborted);
  hiddenRoot.resolve({path:"",entries:[],nextCursor:null});hiddenAlpha.resolve({path:"alpha",entries:[],nextCursor:null});await settle();
  assert.equal(directory("").entries.length,6,"Hidden response must preserve rows");
  const hiddenCount=directoryRequests.length;inline.resumeFiles();assert.equal(directoryRequests.length,hiddenCount);
  document.hidden=false;inline.resumeFiles();
  assert.deepEqual(directoryRequests.slice(-2).map(request=>request.path),["","alpha"]);
  await answer("",rootRows.filter(entry=>entry.path!=="b.txt"));
  await answer("alpha",[{path:"alpha/nested",kind:"directory"},{path:"alpha/refreshed",kind:"file"}]);
  // Refresh visits root and expanded loaded directories; never cached beta.
  await answer("alpha/nested",[]);await answer("gamma",[]);
  assert.equal(directoryRequests.filter(request=>request.path==="beta").length,1,"Refresh must skip collapsed directories");
  assert.equal(directory("alpha").entries.length,2,"Refresh replaces prior pages");
  assert.equal(directory("alpha").details.open,true,"Surviving directories retain expansion");
  releases["b.txt"].resolve(selected);await settle();
  assert.equal(inlineElements["files-title"].textContent,"b.txt","Missing from a page must not clear selection");
  assert.equal(inlineElements["files-content"].children[0],retainedBody,"Unchanged selected bytes retain editor");
  toggle("beta",true);await answer("beta",[]);
  assert.equal(directoryRequests.filter(request=>request.path==="beta").length,2,"Collapsed cache reloads after refresh");

  inline.refreshFiles();
  releases["b.txt"].reject(Object.assign(new Error("This file is unavailable."),{status:404}));await settle();
  assert.equal(inlineElements["files-title"].textContent,"b.txt");assert.equal(inlineElements["files-copy"].disabled,true);
  assert.equal(inlineElements["files-download"].hidden,true);assert.equal(inlineElements["files-content"].children.length,0);
  lastRequest("").reject(Object.assign(new Error("Directory unsupported"),{status:404}));await settle();
  assert.match(directory("").error,/unavailable/);assert.equal(directory("").entries.length,4);
  await answer("alpha",[]);await answer("beta",[]);await answer("gamma",[]);
  statusButton("").handlers.click();lastRequest("").resolve({files:[]});await settle();
  assert.match(directory("").error,/Reload the page/);assert.equal(directory("").entries.length,4,"Invalid old-server shape retains rows");
  inline.pauseFiles();
  console.log("Source file URL, lazy directory, cancellation and selection contracts passed.");
})().catch(error => { console.error(error); process.exitCode = 1; });

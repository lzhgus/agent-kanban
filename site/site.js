(function(){
  document.querySelectorAll('button.copy').forEach(function(b){
    var label=b.textContent;
    b.addEventListener('click',function(){
      var t=b.getAttribute('data-copy');
      var done=function(){b.textContent=b.getAttribute('data-done');setTimeout(function(){b.textContent=label},1600)};
      if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(t).then(done,function(){})}
      else{var a=document.createElement('textarea');a.value=t;document.body.appendChild(a);a.select();try{document.execCommand('copy');done()}catch(e){}a.remove()}
    });
  });
  var v=document.getElementById('promo'),c=document.getElementById('vctl');
  if(!v||!c)return;
  function sync(){var p=v.paused;c.classList.toggle('paused',p);c.setAttribute('aria-label',c.getAttribute(p?'data-play':'data-pause'))}
  c.addEventListener('click',function(){if(v.paused){v.play()}else{v.pause()}});
  v.addEventListener('play',sync);v.addEventListener('pause',sync);
  if(window.matchMedia&&matchMedia('(prefers-reduced-motion: reduce)').matches){v.removeAttribute('autoplay');v.pause()}
  sync();
})();

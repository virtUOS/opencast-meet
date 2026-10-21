(function() {
  var sel = document.getElementById('room');
  if (!sel) return;
  function sync() {
    var opt = sel.options[sel.selectedIndex];
    var wrap = document.getElementById('consent-wrap');
    var cb = document.getElementById('recording_consent');
    var rec = opt && opt.getAttribute('data-record') === 'true';
    wrap.hidden = !rec;
    cb.required = rec;
    if (!rec) cb.checked = false;
  }
  sel.addEventListener('change', sync);
})();

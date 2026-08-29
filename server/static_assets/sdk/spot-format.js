(() => {
  'use strict';

  const formatter = new Intl.NumberFormat();
  const finiteNumber = (value) => {
    const number = Number(value);
    return Number.isFinite(number) && number > 0 ? number : 0;
  };

  const formatCount = (value) => formatter.format(Math.round(finiteNumber(value)));

  const formatBytes = (value) => {
    const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
    let scaled = finiteNumber(value);
    let unit = 0;
    while (scaled >= 1000 && unit < units.length - 1) {
      scaled /= 1000;
      unit++;
    }
    const rounded = unit > 0 && scaled < 10
      ? Math.round(scaled * 10) / 10
      : Math.round(scaled);
    return formatter.format(rounded) + ' ' + units[unit];
  };

  window.SpotFormat = Object.freeze({ formatCount, formatBytes });
})();

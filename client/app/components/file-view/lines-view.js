import React, {Component} from 'react'
import cn from 'classnames'
import {Map} from 'immutable'
import {Tag} from 'antd'

const colorByLevel = (level = '') => {
  switch (level.toLowerCase()) {
    case 'info':
      return 'blue'
    case 'error':
      return 'red'
    case 'warning':
      return 'gold'
  }
}

class LinesView extends Component {
  render() {
    let {lines} = this.props
    return (
      <div className="lines-view-container">
        <div className="lines-view">
          {lines.map((line = Map(), index) => <div key={index} className={cn('line', line.get('level', '').toLowerCase())}>
            {line.get('level') ? <Tag color={colorByLevel(line.get('level'))}>{line.get('level')}</Tag> : null} {line.get('file_name')} {line.get('msg')}</div>)}
        </div>
      </div>
    )
  }
}

export default LinesView
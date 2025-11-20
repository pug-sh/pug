import { Route } from 'wouter'
import AuthProtected from './components/hocs/auth-protected'
import Account from '@/pages/account'
import SigninForm from '@/pages/auth/signin'
import SignupForm from '@/pages/auth/signup'
import Campaigns from '@/pages/campaigns'
import NewCampaign from '@/pages/campaigns/new'
import Dashboard from '@/pages/dashboard'
import Projects from '@/pages/projects'
import ProjectDetail from '@/pages/projects/detail'
import NewProject from '@/pages/projects/new'
import Settings from '@/pages/settings'
import Segments from '@/pages/segments'
import CreateSegment from '@/pages/segments/create'

const Router = () => (
  <>
    <Route path='/signup' component={SignupForm} />
    <Route path='/signin' component={SigninForm} />
    <AuthProtected>
      <Route path='/' component={Dashboard} />
      <Route path='/projects' component={Projects} />
      <Route path='/projects/new' component={NewProject} />
      <Route path='/projects/:id' component={ProjectDetail} />
      <Route path='/settings' component={Settings} />
      <Route path='/account' component={Account} />
      <Route path='/campaigns' component={Campaigns} />
      <Route path='/campaigns/new' component={NewCampaign} />
      <Route path='/segments' component={Segments} />
      <Route path='/segments/new' component={CreateSegment} />
      <Route path='/segments/:id/edit' component={EditSegment} />
    </AuthProtected>
  </>
)

export default Router
